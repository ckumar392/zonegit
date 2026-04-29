// Package merge implements structural 3-way merge over zone trees for
// the `zonegit merge` operation. It compares base/ours/theirs trees
// by walking their entries in lockstep, resolving non-conflicting
// changes automatically and classifying conflicts (both-modified,
// deleted-modified, add-add). Storage I/O is limited to tree and blob
// reads; the merge itself is a pure function of the three root hashes.
package merge
