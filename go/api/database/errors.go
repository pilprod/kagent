package database

import "errors"

// ErrNotFound reports that the requested record does not exist (or is not
// visible to the given user). Match with errors.Is; implementations wrap it
// with call-site context.
var ErrNotFound = errors.New("record not found")

// ErrRuntimeRevisionConflict reports an attempt to reuse an immutable revision
// ID with different digest-owned private policy data.
var ErrRuntimeRevisionConflict = errors.New("runtime revision immutable data conflicts with the stored revision")
