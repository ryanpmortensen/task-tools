PREFIX=/usr/local
DESTDIR=
GOFLAGS=-ldflags "-s -w -X github.com/pcelvng/task-tools.Version=${version} -X github.com/pcelvng/task-tools.BuildTimeUTC=`date -u '+%Y-%m-%d_%I:%M:%S%p'`"
BINDIR=${PREFIX}/bin
BLDDIR = build

ifeq ("${version}", "")
  version=$(shell git describe --tags --always)
endif

EXT=
ifeq (${GOOS},windows)
    EXT=.exe
endif

APPS = backloader crontask files retry filewatcher sort2file deduper batcher http recap filecopy logger stats json2csv flowlord csv2json sql-load sql-readx bq-load transform

all: $(APPS) 

$(BLDDIR)/%: clean
	@mkdir -p $(dir $@)
	CGO_ENABLED=0 GOOS=linux go build ${GOFLAGS} -o ${BLDDIR}/linux/$(@F) ./apps/*/$*
	go build ${GOFLAGS} -o ${BLDDIR}/$(@F) ./apps/*/$*

$(APPS): %: $(BLDDIR)/%

clean:
	rm -rf $(BLDDIR)

.PHONY: install clean all
.PHONY: $(APPS)

install: $(APPS)
	install -m 755 -d ${DESTDIR}${BINDIR}
	for APP in $^ ; do install -m 755 ${BLDDIR}/$$APP ${DESTDIR}${BINDIR}/$$APP${EXT} ; done
	rm -rf build

docker: $(APPS)
	docker build -t hydronica/task-tools:${version} .
	docker push hydronica/task-tools:${version}
