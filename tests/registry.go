package tests

import "context"

// TestFunc is the signature for all compatibility test functions.
type TestFunc func(ctx context.Context, env *Env) TestResult

// All returns every test in execution order.
func All() []TestDef {
	var all []TestDef
	all = append(all, ObjectTests()...)
	all = append(all, BucketTests()...)
	all = append(all, VersioningTests()...)
	all = append(all, ObjectLockTests()...)
	all = append(all, MultipartTests()...)
	all = append(all, PresignedTests()...)
	all = append(all, STSTests()...)
	return all
}
