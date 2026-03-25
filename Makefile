BINARY_NAME=tfm
INSTALL_PATH=/usr/local/bin/$(BINARY_NAME)

all: build

build:
	go build -o $(BINARY_NAME) main.go

install: build
	sudo mv $(BINARY_NAME) $(INSTALL_PATH)

clean:
	rm -f $(BINARY_NAME)

.PHONY: all build install clean
