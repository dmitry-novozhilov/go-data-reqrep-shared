GO ?= go

.PHONY: update-header generate test bench

update-header:
	curl -fsSL https://raw.githubusercontent.com/vividsnow/perl5-data-reqrep-shared/master/reqrep.h \
	    -o gen/reqrep.h
	$(GO) generate ./...

generate:
	$(GO) generate ./...

test:
	$(GO) test -v -race ./...

bench:
	$(GO) test -bench=. -benchmem ./...
