// This file keeps the Go toolchain out of this directory. There is no Go
// code here, but node_modules can hold some (a dependency of the linter
// ships a Go package), and a directory with a go.mod of its own is not part
// of ./... in the repository root.
module github.com/thanoslabs/beacon/web

go 1.27.4
