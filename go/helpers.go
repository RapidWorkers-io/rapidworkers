package rapidworkers

// Optional arguments are pointers, so an unset argument is left out of the
// call while a zero value (false, 0, "") is still sent. These helpers return a
// pointer to their argument:
//
//	rw.ListJobs(ctx, rapidworkers.ListJobsParams{Page: rapidworkers.Int(2)})

// String returns a pointer to v.
func String(v string) *string { return &v }

// Int returns a pointer to v.
func Int(v int) *int { return &v }

// Float returns a pointer to v.
func Float(v float64) *float64 { return &v }

// Bool returns a pointer to v.
func Bool(v bool) *bool { return &v }

// Ptr returns a pointer to v, for any type.
func Ptr[T any](v T) *T { return &v }
