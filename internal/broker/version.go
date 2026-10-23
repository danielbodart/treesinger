package broker

// Version is what this build calls itself, replaced at build time with
//
//	go build -ldflags "-X github.com/danielbodart/treesinger/internal/broker.Version=<version>"
//
// The fallback is what you get from `go run .`, where there was no build step
// to bake anything in. It says so, rather than guessing a number that would
// then appear in a bug report as if it had been released.
var Version = "development"
