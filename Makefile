TITLE ?= vimee
SUBTITLE ?= A headless vim engine for the web

.PHONY: build generate clean

build:
	go build -o opengraph .

generate: build
	./opengraph "$(TITLE)" "$(SUBTITLE)"

clean:
	rm -f opengraph og.png
