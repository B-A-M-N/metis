.PHONY: build test clean

# -buildvcs=false is required on pop-os: plain `go build` fails there
# with "error obtaining VCS status: exit status 128", because Go
# tries to stamp VCS info and git reports the folder is not a git
# repository (exit 128). Disabling stamping changes nothing else
# about the binary; it just skips that stamp.
build:
	go build -buildvcs=false -o tui-dash .

test:
	go test ./...

clean:
	rm -f tui-dash
