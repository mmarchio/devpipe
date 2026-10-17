package statefile

// Package statefile contains the logical state-file type and scope domain
// (WBS 7.1/7.3). Pure domain: no persistence, no REST, no filesystem.
// Logical state-file types are identified by normalized keys, never by
// literal filenames; filenames are defaults carried on the type.
