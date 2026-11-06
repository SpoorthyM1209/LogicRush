.PHONY: all build test clean

all: build

build:
	go build -o kvdb ./src/cmd/kvdb
	go build -o torture ./torture

test:
	go test -v ./tests

clean:
	rm -f kvdb kvdb.exe torture torture.exe *.db
