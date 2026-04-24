// Package repo is the public Go API of zonegit. It composes pkg/store,
// pkg/object, pkg/zone, pkg/refs, and pkg/history into a single Repo
// type with high-level operations that the CLI and server consume.
//
// Anything embedding zonegit should depend on
// this package, never on the layers below.
package repo
