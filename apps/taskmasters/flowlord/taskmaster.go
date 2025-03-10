package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	gtools "github.com/jbsmith7741/go-tools"
	"github.com/pcelvng/task"
	"github.com/pcelvng/task/bus"
	"github.com/pkg/errors"
	"github.com/robfig/cron/v3"

	"github.com/pcelvng/task-tools/bootstrap"
	"github.com/pcelvng/task-tools/file"
	"github.com/pcelvng/task-tools/slack"
	"github.com/pcelvng/task-tools/tmpl"
	"github.com/pcelvng/task-tools/workflow"
)

func init() {
	rand.Seed(time.Now().Unix())
}

type taskMaster struct {
	initTime      time.Time
	nextUpdate    time.Time
	lastUpdate    time.Time
	path          string
	dur           time.Duration
	producer      bus.Producer
	doneConsumer  bus.Consumer
	filesConsumer bus.Consumer
	fOpts         *file.Options
	doneTopic     string
	failedTopic   string
	*workflow.Cache
	cron        *cron.Cron
	slack       *slack.Slack
	files       []fileRule
	refreshChan chan struct{}
}

type stats struct {
	RunTime    string `json:"runtime"`
	NextUpdate string `json:"next_cache"`
	LastUpdate string `json:"last_cache"`

	Workflow map[string]map[string]cEntry `json:"workflow"`
}

type cEntry struct {
	Next     *time.Time `json:"Next,omitempty"`
	Prev     *time.Time `json:"Prev,omitempty"`
	Warning  string     `json:"warning,omitempty"`
	Schedule []string   `json:"Schedule,omitempty"`
	Child    []string   `json:"Child,omitempty"`
}

func New(app *bootstrap.TaskMaster) bootstrap.Runner {
	opts := app.AppOpt().(*options)
	bOpts := app.GetBusOpts()
	bOpts.InTopic = opts.DoneTopic
	if bOpts.Bus == "pubsub" {
		bOpts.InChannel = opts.DoneTopic + "-flowlord"
	}
	consumer, err := bus.NewConsumer(bOpts)
	if err != nil {
		log.Fatal("doneConsumer init", err)
	}
	tm := &taskMaster{
		initTime:     time.Now(),
		path:         opts.Workflow,
		doneTopic:    opts.DoneTopic,
		failedTopic:  opts.FailedTopic,
		fOpts:        opts.File,
		producer:     app.NewProducer(),
		doneConsumer: consumer,
		cron:         cron.New(cron.WithSeconds()),
		dur:          opts.Refresh,
		slack:        opts.Slack,
		refreshChan:  make(chan struct{}),
	}
	if opts.FileTopic != "" {
		bOpts.InTopic = opts.FileTopic
		bOpts.InChannel = opts.FileTopic + "-flowlord"
		tm.filesConsumer, err = bus.NewConsumer(bOpts)
		if err != nil {
			log.Println("files consumer: ", err)
			tm.filesConsumer = nil
		}
	}
	http.HandleFunc("/refresh", tm.refreshCache)
	return tm
}

// pName creates the phase task name
// topic:job
// topic
func pName(topic, job string) string {
	if job == "" {
		return topic
	}
	return topic + ":" + job
}

func (tm *taskMaster) Info() interface{} {
	sts := stats{
		RunTime:    gtools.PrintDuration(time.Now().Sub(tm.initTime)),
		NextUpdate: tm.nextUpdate.Format("2006-01-02T15:04:05"),
		LastUpdate: tm.lastUpdate.Format("2006-01-02T15:04:05"),
		Workflow:   make(map[string]map[string]cEntry),
	}

	// create a copy of all workflows
	wCache := make(map[string]map[string]workflow.Phase) // [file][task:job]Phase
	for key, w := range tm.Cache.Workflows {
		phases := make(map[string]workflow.Phase)
		for _, j := range w.Phases {
			phases[pName(j.Topic(), j.Job())] = j
		}
		wCache[key] = phases
	}

	for _, e := range tm.cron.Entries() {
		j, ok := e.Job.(*job)
		if !ok {
			continue
		}
		ent := cEntry{
			Next:     &e.Next,
			Prev:     &e.Prev,
			Schedule: []string{j.Schedule + "?offset=" + gtools.PrintDuration(j.Offset)},
			Child:    make([]string, 0),
		}
		k := pName(j.Topic, j.Name)

		w, found := sts.Workflow[j.Workflow]
		if !found {
			w = make(map[string]cEntry)
			sts.Workflow[j.Workflow] = w
		}

		// check if for multi-scheduled entries
		if e, found := w[k]; found {
			if e.Prev.After(*ent.Prev) {
				ent.Prev = e.Prev // keep the last run time
			}
			if e.Next.Before(*ent.Next) {
				ent.Next = e.Next // keep the next run time
			}
			ent.Schedule = append(ent.Schedule, e.Schedule...)
		}
		// add children
		ent.Child = tm.getAllChildren(j.Topic, j.Workflow, j.Name)
		w[k] = ent

		// remove entries from wCache
		delete(wCache[j.Workflow], k)
		for _, child := range ent.Child {
			for _, v := range strings.Split(child, " ➞ ") {
				delete(wCache[j.Workflow], v)
			}
		}
	}

	// add files based tasks

	for _, f := range tm.files {
		wPath := f.workflowFile
		w, found := sts.Workflow[wPath]
		if !found {
			w = make(map[string]cEntry)
			sts.Workflow[wPath] = w
		}
		k := pName(f.Topic(), f.Job())
		ent := cEntry{
			Schedule: []string{f.SrcPattern},
			Child:    tm.getAllChildren(f.Topic(), f.workflowFile, f.Job()),
		}
		w[k] = ent

		// remove entries from wCache
		delete(wCache[f.workflowFile], k)
		for _, child := range ent.Child {
			for _, v := range strings.Split(child, " ➞ ") {
				delete(wCache[f.workflowFile], v)
			}
		}
	}

	// Add non cron based tasks
	for f, w := range wCache {
		for _, v := range w {
			k := pName(v.Topic(), v.Job())
			// check for parents
			for v.DependsOn != "" {
				if t, found := wCache[f][v.DependsOn]; found {
					k = v.DependsOn
					v = t
				} else {
					break
				}

			}

			children := tm.getAllChildren(v.Topic(), f, v.Job())
			// todo: remove children from Cache
			if _, found := sts.Workflow[f]; !found {
				sts.Workflow[f] = make(map[string]cEntry)
			}
			warning := validatePhase(v)
			if v.DependsOn != "" {
				warning += "parent task not found: " + v.DependsOn
			}
			sts.Workflow[f][k] = cEntry{
				Schedule: make([]string, 0),
				Warning:  warning,
				Child:    children,
			}
		}
	}

	return sts
}

func (tm *taskMaster) refreshCache(w http.ResponseWriter, _ *http.Request) {
	tm.refreshChan <- struct{}{}
	w.Write([]byte("cache refreshed"))
}

func (tm *taskMaster) getAllChildren(topic, workflow, job string) (s []string) {
	for _, c := range tm.Children(task.Task{Type: topic, Meta: "workflow=" + workflow + "&job=" + job}) {
		job := strings.Trim(c.Task+":"+c.Job(), ":")
		if children := tm.getAllChildren(c.Task, workflow, c.Job()); len(children) > 0 {
			job += " ➞ " + strings.Join(children, " ➞ ")
		}
		s = append(s, job)
	}
	return s
}

// AutoUpdate will create a go routine to auto update the cached files
// if any changes have been made to the workflow files
func (tm *taskMaster) AutoUpdate() {
	go func() {
		tm.refreshChan <- struct{}{}
		ticker := time.NewTicker(tm.dur)
		for range ticker.C {
			tm.refreshChan <- struct{}{}
		}
	}()
	for range tm.refreshChan {
		files, err := tm.Cache.Refresh()
		if err != nil {
			log.Println("error reloading workflow files", err)
			return
		}
		// if there are values in files, there are changes that need to be reloaded
		if len(files) > 0 {
			log.Println("reloading workflow changes")
			tcron := tm.cron
			tm.cron = cron.New(cron.WithSeconds())
			if err := tm.schedule(); err != nil {
				log.Println("error setting up cron schedule", err)
				tm.cron = tcron
			} else {
				tcron.Stop()
			}
		}
		tm.lastUpdate = time.Now()
		tm.nextUpdate = tm.lastUpdate.Add(tm.dur)
	}
}

func (tm *taskMaster) Run(ctx context.Context) (err error) {
	if tm.Cache, err = workflow.New(tm.path, tm.fOpts); err != nil {
		return errors.Wrapf(err, "workflow setup")
	}

	// refresh the workflow if the file(s) have been changed
	go tm.AutoUpdate()

	if err := tm.schedule(); err != nil {
		return errors.Wrapf(err, "cron schedule")
	}

	go tm.readDone(ctx)
	go tm.readFiles(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return nil
		}
	}
}

func validatePhase(p workflow.Phase) string {

	if p.DependsOn == "" {
		if p.Rule == "" {
			return "invalid phase: rule and dependsOn are blank"
		}
		// verify at least one valid rule is there
		rules, _ := url.ParseQuery(p.Rule)
		if rules.Get("cron") == "" {
			return fmt.Sprintf("no valid rule found: %v", p.Rule)
		}

		return ""

	}
	// DependsOn != ""
	if p.Rule != "" {
		return fmt.Sprintf("ignored rule: %v", p.Rule)
	}

	return ""
}

// schedule the tasks and refresh the schedule when updated
func (tm *taskMaster) schedule() (err error) {
	if len(tm.Workflows) == 0 {
		return fmt.Errorf("no workflows found check path %s", tm.path)
	}
	for path, workflow := range tm.Workflows {
		for _, w := range workflow.Parent() {
			rules, _ := url.ParseQuery(w.Rule)
			if f := rules.Get("files"); f != "" {
				//todo: add to log checker
				r := fileRule{
					SrcPattern:   f,
					workflowFile: path,
					Phase:        w,
					CronCheck:    rules.Get("cron"),
				}
				r.CountCheck, _ = strconv.Atoi(rules.Get("count"))

				tm.files = append(tm.files, r)

				//todo: Create a cron job for a task that is cron and files
			}

			if rules.Get("cron") == "" {
				log.Printf("skip: task:%s, rule:%s", w.Task, w.Rule)
				continue
			}

			j := &job{
				Name:     rules.Get("job"),
				Workflow: path,
				Topic:    w.Task,
				Schedule: rules.Get("cron"),
				Template: w.Template,
				producer: tm.producer,
			}
			if s := rules.Get("offset"); s != "" {
				j.Offset, err = time.ParseDuration(s)
				if err != nil {
					return errors.Wrapf(err, "invalid duration %s", s)
				}
			}

			if _, err = tm.cron.AddJob(j.Schedule, j); err != nil {
				return errors.Wrapf(err, "invalid rule for %s:%s %s", path, w.Task, w.Rule)
			}
		}
	}
	tm.cron.Start()
	return nil
}

// Process the given task
// 1. check if the task needs to be retried
// 2. start any downstream tasks
// Send retry failed tasks to tm.failedTopic (only if the phase exists in the workflow)
func (tm *taskMaster) Process(t *task.Task) error {
	meta, _ := url.ParseQuery(t.Meta)

	// attempt to retry
	if t.Result == task.ErrResult {
		p := tm.Get(*t)
		rules, _ := url.ParseQuery(p.Rule)

		r := meta.Get("retry")
		i, _ := strconv.Atoi(r)
		// the task should have a workflow phase
		if p.Task == "" {
			return nil
		}
		if p.Retry > i {
			var delay time.Duration
			if s := rules.Get("retry_delay"); s != "" {
				delay, _ = time.ParseDuration(s)
				delay = delay + jitterPercent(delay, 40)
				meta.Set("delayed", gtools.PrintDuration(delay))
			}
			t = task.NewWithID(t.Type, t.Info, t.ID)
			i++
			meta.Set("retry", strconv.Itoa(i))
			t.Meta = meta.Encode()
			go func() {
				time.Sleep(delay)
				if err := tm.producer.Send(t.Type, t.JSONBytes()); err != nil {
					log.Println(err)
				}
			}()
			return nil
		} else if tm.failedTopic != "-" {
			// send to the retry failed topic if retries > p.Retry
			meta.Set("retry", "failed")
			t.Meta = meta.Encode()
			tm.producer.Send(tm.failedTopic, t.JSONBytes())
			if tm.slack != nil {
				b, _ := json.MarshalIndent(t, "", "  ")
				tm.slack.Notify(string(b), slack.Critical)
			}
		}

		return nil
	}

	// start off any children tasks
	if t.Result == task.CompleteResult {
		for _, p := range tm.Children(*t) {
			if !isReady(p.Rule, t.Meta) {
				continue
			}
			info := tmpl.Meta(p.Template, meta)
			rules, _ := url.ParseQuery(p.Rule)

			taskTime := tmpl.InfoTime(t.Info)
			if v := meta.Get("cron"); v != "" && taskTime.IsZero() {
				taskTime, _ = time.Parse(time.RFC3339, v)
			}
			if !taskTime.IsZero() {
				info = tmpl.Parse(info, taskTime)
			}
			child := task.NewWithID(p.Task, info, t.ID)

			child.Meta = "workflow=" + meta.Get("workflow")
			if v := meta.Get("cron"); v != "" {
				child.Meta += "&cron=" + v
			}
			if rules.Get("job") != "" {
				child.Meta += "&job=" + rules.Get("job")
			}
			if err := tm.producer.Send(p.Task, child.JSONBytes()); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown result %q %s", t.Result, t.JSONString())
}

var regexMeta = regexp.MustCompile(`{meta:(\w+)}`)

// isReady checks a task rule for any require fields and verifies
// that all fields are included and valid
func isReady(rule, meta string) bool {
	rules, _ := url.ParseQuery(rule)
	met, _ := url.ParseQuery(meta)
	req := strings.Join(rules["require"], ",")
	for _, m := range regexMeta.FindAllStringSubmatch(req, -1) {
		if s := met.Get(m[1]); s == "" {
			return false
		}
	}
	return true
}

func (tm *taskMaster) readDone(ctx context.Context) {
	for {
		b, done, err := tm.doneConsumer.Msg()
		if done || task.IsDone(ctx) {
			log.Println("stopping done Consumer")
			return
		}
		if err != nil {
			log.Println("done Consumer", err)
			return
		}
		t := &task.Task{}
		if err = json.Unmarshal(b, t); err != nil {
			log.Printf("unmarshal error %q: %s", string(b), err)
			continue
		}
		if err := tm.Process(t); err != nil {
			log.Println(err)
		}
	}
}

func (tm *taskMaster) readFiles(ctx context.Context) {
	if tm.filesConsumer == nil {
		log.Println("no files consumer")
		return
	}
	for {
		b, done, err := tm.filesConsumer.Msg()
		if done || task.IsDone(ctx) {
			log.Println("stopping files Consumer")
			return
		}
		if err != nil {
			log.Println("files Consumer", err)
			return
		}
		s := unmarshalStat(b)
		if err := tm.matchFile(s); err != nil {
			log.Println("files: ", err)
		}
	}
}

// jitterPercent will return a time.Duration representing extra
// 'jitter' to be added to the wait time. Jitter is important
// in retry events since the original cause of failure can be
// due to too many jobs being processed at a time.
//
// By adding some jitter the retry events won't all happen
// at once but will get staggered to prevent the problem
// from happening again.
//
// 'p' is a percentage of the wait time. Duration returned
// is a random duration between 0 and p. 'p' should be a value
// between 0-100.
func jitterPercent(wait time.Duration, p int64) time.Duration {
	// p == 40
	if wait == 0 {
		return 0
	}
	maxJitter := (int64(wait) * p) / 100

	return time.Duration(rand.Int63n(maxJitter))
}
