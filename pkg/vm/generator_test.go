package vm

import (
	"context"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/risor/v2/pkg/object"
)

// genPull drives one next() call on a generator object and returns the
// (value, done) pair from the script-visible result map.
func genPull(t *testing.T, ctx context.Context, g *object.Generator) (object.Object, bool) {
	t.Helper()
	res, err := g.Next(ctx)
	if err != nil {
		t.Fatalf("generator next() failed: %v", err)
	}
	value, done, err := readGeneratorResult(res)
	if err != nil {
		t.Fatalf("generator result invalid: %v", err)
	}
	return value, done
}

// genPullErr drives next() and expects an error.
func genPullErr(t *testing.T, ctx context.Context, g *object.Generator) error {
	t.Helper()
	if _, err := g.Next(ctx); err == nil {
		t.Fatal("expected generator next() to return an error")
		return nil
	} else {
		return err
	}
}

func asInt(t *testing.T, obj object.Object) int64 {
	t.Helper()
	i, ok := obj.(*object.Int)
	if !ok {
		t.Fatalf("expected int, got %s", obj.Inspect())
	}
	return i.Value()
}

func asString(t *testing.T, obj object.Object) string {
	t.Helper()
	s, ok := obj.(*object.String)
	if !ok {
		t.Fatalf("expected string, got %s", obj.Inspect())
	}
	return s.Value()
}

func TestGeneratorBasicSequence(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() {
		yield 1
		yield 2
		yield 3
	}
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := result.(*object.Generator)
	if !ok {
		t.Fatalf("expected generator object, got %T", result)
	}
	if g.Type() != object.GENERATOR {
		t.Fatalf("expected generator type, got %s", g.Type())
	}

	ctx := context.Background()
	// Calling the function must not run the body.
	v, done := genPull(t, ctx, g)
	if done || asInt(t, v) != 1 {
		t.Fatalf("first pull = (%v,%v), want (1,false)", v, done)
	}
	if asInt(t, mustPull(t, ctx, g)) != 2 {
		t.Fatal("second pull should yield 2")
	}
	if asInt(t, mustPull(t, ctx, g)) != 3 {
		t.Fatal("third pull should yield 3")
	}
	// Natural completion: {value: null, done: true}.
	v, done = genPull(t, ctx, g)
	if !done {
		t.Fatal("expected done=true after exhaustion")
	}
	if _, isNil := v.(*object.NilType); !isNil {
		t.Fatalf("natural completion value must be nil, got %s", v.Inspect())
	}
	// Later pulls stay done and do not run the body again.
	v, done = genPull(t, ctx, g)
	if !done {
		t.Fatal("expected stable done=true")
	}
}

func mustPull(t *testing.T, ctx context.Context, g *object.Generator) object.Object {
	t.Helper()
	v, done := genPull(t, ctx, g)
	if done {
		t.Fatal("unexpected done=true")
	}
	return v
}

func TestGeneratorExplicitReturn(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() {
		yield 1
		return 42
		yield 3
	}
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, g)) != 1 {
		t.Fatal("want 1")
	}
	v, done := genPull(t, ctx, g)
	if !done || asInt(t, v) != 42 {
		t.Fatalf("completion = (%s,%v), want (42,true)", v.Inspect(), done)
	}
	// The termination value is not produced again; later pulls stay done.
	v, done = genPull(t, ctx, g)
	if !done {
		t.Fatal("want done after return")
	}
}

func TestGeneratorLazySideEffects(t *testing.T) {
	result, err := run(context.Background(), `
	let log = []
	function* gen(a) {
		log.append("start")
		yield a
		log.append("middle")
		yield a + 1
		log.append("end")
	}
	[gen(100), log]
	`)
	if err != nil {
		t.Fatal(err)
	}
	list := result.(*object.List).Value()
	g := list[0].(*object.Generator)
	log := list[1].(*object.List)
	if len(log.Value()) != 0 {
		t.Fatal("generator body must not run before first next()")
	}
	ctx := context.Background()
	mustPull(t, ctx, g)
	if got := log.Inspect(); got != `["start"]` {
		t.Fatalf("after first pull log = %s, want [\"start\"]", got)
	}
	mustPull(t, ctx, g)
	if got := log.Inspect(); got != `["start", "middle"]` {
		t.Fatalf("after second pull log = %s", got)
	}
	genPull(t, ctx, g)
	if got := log.Inspect(); got != `["start", "middle", "end"]` {
		t.Fatalf("after completion log = %s", got)
	}
	// Prefix effects must never be replayed.
	genPull(t, ctx, g)
	genPull(t, ctx, g)
	if len(log.Value()) != 3 {
		t.Fatalf("prefix effects replayed: %s", log.Inspect())
	}
}

func TestGeneratorParametersAndLocals(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen(required, withDefault, flag=true, ...rest) {
		yield required
		yield withDefault
		yield flag
		yield len(rest)
		yield rest[1]
		let local = required * 10
		yield local
	}
	gen(1, 2, false, 10, 20, 30)
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, g)) != 1 {
		t.Fatal("required")
	}
	if asInt(t, mustPull(t, ctx, g)) != 2 {
		t.Fatal("default-position arg")
	}
	if flag := mustPull(t, ctx, g); flag == nil || flag.IsTruthy() {
		t.Fatalf("flag should be false, got %s", flag.Inspect())
	}
	if asInt(t, mustPull(t, ctx, g)) != 3 {
		t.Fatal("rest length")
	}
	if asInt(t, mustPull(t, ctx, g)) != 20 {
		t.Fatal("rest[1]")
	}
	if asInt(t, mustPull(t, ctx, g)) != 10 {
		t.Fatal("local")
	}
}

func TestGeneratorInvalidArgsAtCreation(t *testing.T) {
	_, err := run(context.Background(), `
	function* gen(a, b) { yield a }
	gen(1)
	`)
	if err == nil || (!strings.Contains(err.Error(), "takes") && !strings.Contains(err.Error(), "argument")) {
		t.Fatalf("expected args error at call time, got %v", err)
	}
}

func TestGeneratorLocalStateAcrossYields(t *testing.T) {
	result, err := run(context.Background(), `
	function* counter() {
		let n = 0
		if (true) {
			n++
			yield n
		}
		n++
		yield n
		n = n + 10
		yield n
	}
	counter()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, g)) != 1 ||
		asInt(t, mustPull(t, ctx, g)) != 2 ||
		asInt(t, mustPull(t, ctx, g)) != 12 {
		t.Fatal("local state did not survive yields")
	}
}

func TestGeneratorOrdinaryCallsInBody(t *testing.T) {
	result, err := run(context.Background(), `
	function add(a, b) { return a + b }
	function* gen() {
		yield add(2, 3)
		let x = 10
		yield add(x, 5)
	}
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, g)) != 5 ||
		asInt(t, mustPull(t, ctx, g)) != 15 {
		t.Fatal("ordinary calls inside generator failed")
	}
}

func TestGeneratorNamedRecursion(t *testing.T) {
	result, err := run(context.Background(), `
	function* countDown(n) {
		if (n == 0) { return }
		yield n
		yield* countDown(n - 1)
	}
	countDown(3)
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	for _, w := range []int64{3, 2, 1} {
		if asInt(t, mustPull(t, ctx, g)) != w {
			t.Fatal("recursive generator failed")
		}
	}
	if _, done := genPull(t, ctx, g); !done {
		t.Fatal("recursive generator should finish")
	}
}

func TestMultipleGeneratorsInterleave(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen(base) {
		let n = base
		yield n
		n++
		yield n
	}
	[gen(100), gen(200)]
	`)
	if err != nil {
		t.Fatal(err)
	}
	gens := result.(*object.List).Value()
	a := gens[0].(*object.Generator)
	b := gens[1].(*object.Generator)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, a)) != 100 {
		t.Fatal("a1")
	}
	if asInt(t, mustPull(t, ctx, b)) != 200 {
		t.Fatal("b1")
	}
	if asInt(t, mustPull(t, ctx, a)) != 101 {
		t.Fatal("a2")
	}
	if asInt(t, mustPull(t, ctx, b)) != 201 {
		t.Fatal("b2")
	}
	if _, done := genPull(t, ctx, a); !done {
		t.Fatal("a done")
	}
	if _, done := genPull(t, ctx, b); !done {
		t.Fatal("b done")
	}
}

func TestGeneratorClosureCapturesShared(t *testing.T) {
	result, err := run(context.Background(), `
	function makePair() {
		let shared = { n: 0 }
		function inc() { shared.n++ }
		function* gen() {
			inc()
			yield shared.n
			inc()
			yield shared.n
		}
		return [gen(), shared]
	}
	makePair()
	`)
	if err != nil {
		t.Fatal(err)
	}
	list := result.(*object.List).Value()
	g := list[0].(*object.Generator)
	shared := list[1].(*object.Map)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, g)) != 1 {
		t.Fatal("closure side effect 1")
	}
	if asInt(t, mustPull(t, ctx, g)) != 2 {
		t.Fatal("closure side effect 2")
	}
	v, _ := shared.GetItem(object.NewString("n"))
	if asInt(t, v) != 2 {
		t.Fatal("generator closure must operate on the shared captured reference")
	}
}

func TestGeneratorsDoNotShareCells(t *testing.T) {
	result, err := run(context.Background(), `
	function maker(base) {
		let count = 0
		return function* () {
			count++
			yield base + count
			count++
			yield base + count
		}
	}
	let a = maker(0)
	let b = maker(100)
	[a(), b()]
	`)
	if err != nil {
		t.Fatal(err)
	}
	gens := result.(*object.List).Value()
	a := gens[0].(*object.Generator)
	b := gens[1].(*object.Generator)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, a)) != 1 {
		t.Fatal("a1")
	}
	if asInt(t, mustPull(t, ctx, b)) != 101 {
		t.Fatal("b must have independent captured locals")
	}
	if asInt(t, mustPull(t, ctx, a)) != 2 {
		t.Fatal("a2")
	}
	if asInt(t, mustPull(t, ctx, b)) != 102 {
		t.Fatal("b2")
	}
}

func TestGeneratorDelegation(t *testing.T) {
	result, err := run(context.Background(), `
	function* inner() {
		yield 2
		yield 3
	}
	function* gen() {
		yield 1
		yield* inner()
		yield* [4, 5]
		yield* range(6, 8)
		yield 9
	}
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	for i, w := range []int64{1, 2, 3, 4, 5, 6, 7, 9} {
		v := mustPull(t, ctx, g)
		if asInt(t, v) != w {
			t.Fatalf("pull %d = %s, want %d", i, v.Inspect(), w)
		}
	}
	if _, done := genPull(t, ctx, g); !done {
		t.Fatal("delegating generator should finish")
	}
}

func TestGeneratorDelegationEmpty(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() {
		yield 1
		yield* []
		yield* range(0, 0)
		yield 2
	}
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, g)) != 1 ||
		asInt(t, mustPull(t, ctx, g)) != 2 {
		t.Fatal("empty delegate should continue immediately")
	}
	if _, done := genPull(t, ctx, g); !done {
		t.Fatal("done")
	}
}

func TestGeneratorDelegationMap(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() { yield* {"b": 1, "a": 2} }
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	// Map delegation yields keys in stable sorted order, matching spread.
	if asString(t, mustPull(t, ctx, g)) != "a" {
		t.Fatal("map delegate key 1")
	}
	if asString(t, mustPull(t, ctx, g)) != "b" {
		t.Fatal("map delegate key 2")
	}
	if _, done := genPull(t, ctx, g); !done {
		t.Fatal("done")
	}
}

func TestGeneratorDelegationHostIter(t *testing.T) {
	// A host-built pull iterator (object.NewIter) is delegated item by item
	// without collecting the sequence up front.
	var produced []int64
	hostIter := object.NewIter("test", func(ctx context.Context, fn func(key, value object.Object) bool) {
		for _, n := range []int64{7, 8, 9} {
			produced = append(produced, n)
			if !fn(object.NewInt(int64(n - 7)), object.NewInt(n)) {
				return
			}
		}
	})
	result, err := run(context.Background(), `
	function* gen() {
		yield 1
		yield* hostIter
		yield 10
	}
	gen()
	`, runOpts{Globals: map[string]any{"hostIter": hostIter}})
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, g)) != 1 {
		t.Fatal("pre-iter yield")
	}
	// After pulling one delegated item, exactly one host item was produced.
	if asInt(t, mustPull(t, ctx, g)) != 7 {
		t.Fatal("delegated 7")
	}
	if len(produced) != 1 {
		t.Fatalf("host producer ran ahead: produced %v", produced)
	}
	if asInt(t, mustPull(t, ctx, g)) != 8 {
		t.Fatal("delegated 8")
	}
	if asInt(t, mustPull(t, ctx, g)) != 9 {
		t.Fatal("delegated 9")
	}
	if asInt(t, mustPull(t, ctx, g)) != 10 {
		t.Fatal("post-iter yield")
	}
	if _, done := genPull(t, ctx, g); !done {
		t.Fatal("done")
	}
}

func TestGeneratorDelegationChildReturnNotYielded(t *testing.T) {
	result, err := run(context.Background(), `
	function* inner() { yield 1; return 99 }
	function* outer() {
		yield 0
		yield* inner()
		yield 2
	}
	outer()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	for _, w := range []int64{0, 1, 2} {
		if asInt(t, mustPull(t, ctx, g)) != w {
			t.Fatal("child return value must not become an outer yield")
		}
	}
	if _, done := genPull(t, ctx, g); !done {
		t.Fatal("done")
	}
}

func TestGeneratorDelegationNonEnumerable(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() { yield* 42 }
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	err = genPullErr(t, context.Background(), g)
	if !strings.Contains(err.Error(), "yield*") {
		t.Fatalf("expected type error about yield*, got %v", err)
	}
}

func TestGeneratorUnhandledError(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() {
		yield 1
		throw "boom"
	}
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, g)) != 1 {
		t.Fatal("yield 1")
	}
	err = genPullErr(t, ctx, g)
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error message must be preserved, got %v", err)
	}
	// The generator is now terminal: next() reports done=true without error
	// and does not re-run anything.
	v, done := genPull(t, ctx, g)
	if !done {
		t.Fatal("errored generator must be done")
	}
	if _, isNil := v.(*object.NilType); !isNil {
		t.Fatal("post-error done value must be nil")
	}
	v, done = genPull(t, ctx, g)
	if !done {
		t.Fatal("done must be stable after error")
	}
	// close() after an error is a successful no-op.
	if _, err := g.Close(ctx); err != nil {
		t.Fatalf("close after error must succeed: %v", err)
	}
}

func TestGeneratorYieldInCatch(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() {
		try {
			yield 1
			throw "x"
		} catch e {
			yield e
			yield 3
		}
		yield 4
	}
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	if asInt(t, mustPull(t, ctx, g)) != 1 {
		t.Fatal("try yield")
	}
	if got := asString(t, mustPull(t, ctx, g)); got != "x" {
		t.Fatalf("catch yield = %q, want x", got)
	}
	if asInt(t, mustPull(t, ctx, g)) != 3 || asInt(t, mustPull(t, ctx, g)) != 4 {
		t.Fatal("post-catch yields")
	}
	if _, done := genPull(t, ctx, g); !done {
		t.Fatal("done")
	}
}

func TestGeneratorFinallyOnReturnAndError(t *testing.T) {
	// Explicit return runs finally before completion.
	result, err := run(context.Background(), `
	let log = []
	function* gen() {
		try {
			yield 1
			return 5
		} finally {
			log.append("done")
		}
	}
	[gen(), log]
	`)
	if err != nil {
		t.Fatal(err)
	}
	list := result.(*object.List).Value()
	g := list[0].(*object.Generator)
	log := list[1].(*object.List)
	ctx := context.Background()
	mustPull(t, ctx, g)
	if len(log.Value()) != 0 {
		t.Fatal("finally must not run at yield")
	}
	v, done := genPull(t, ctx, g)
	if !done || asInt(t, v) != 5 {
		t.Fatal("return completion")
	}
	if log.Inspect() != `["done"]` {
		t.Fatalf("finally should run on return: %s", log.Inspect())
	}
}

func TestGeneratorCloseNeverStarted(t *testing.T) {
	result, err := run(context.Background(), `
	let log = []
	function* gen() {
		log.append("body")
		yield 1
	}
	[gen(), log]
	`)
	if err != nil {
		t.Fatal(err)
	}
	list := result.(*object.List).Value()
	g := list[0].(*object.Generator)
	log := list[1].(*object.List)
	ctx := context.Background()
	if _, err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(log.Value()) != 0 {
		t.Fatal("close on an unstarted generator must not run the body")
	}
	if _, done := genPull(t, ctx, g); !done {
		t.Fatal("next after close must be done")
	}
	// Repeated close is fine.
	if _, err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratorCloseSkipsBusinessAndRunsFinally(t *testing.T) {
	result, err := run(context.Background(), `
	let log = []
	function* gen() {
		try {
			yield 1
			log.append("after-yield")
			yield 2
		} finally {
			log.append("finally")
		}
	}
	[gen(), log]
	`)
	if err != nil {
		t.Fatal(err)
	}
	list := result.(*object.List).Value()
	g := list[0].(*object.Generator)
	log := list[1].(*object.List)
	ctx := context.Background()
	mustPull(t, ctx, g)
	if _, err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := log.Inspect(); got != `["finally"]` {
		t.Fatalf("close must skip business and run only finally, got %s", got)
	}
	// Idempotent: cleanup is not executed twice.
	if _, err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := log.Inspect(); got != `["finally"]` {
		t.Fatalf("cleanup ran twice: %s", got)
	}
	if _, done := genPull(t, ctx, g); !done {
		t.Fatal("next after close done")
	}
}

func TestGeneratorCloseNestedFinally(t *testing.T) {
	result, err := run(context.Background(), `
	let log = []
	function* gen() {
		try {
			try {
				yield 1
			} finally {
				log.append("inner")
			}
		} finally {
			log.append("outer")
		}
	}
	[gen(), log]
	`)
	if err != nil {
		t.Fatal(err)
	}
	list := result.(*object.List).Value()
	g := list[0].(*object.Generator)
	log := list[1].(*object.List)
	ctx := context.Background()
	mustPull(t, ctx, g)
	if _, err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := log.Inspect(); got != `["inner", "outer"]` {
		t.Fatalf("nested finally must run inner to outer: %s", got)
	}
}

func TestGeneratorCloseWithoutFinally(t *testing.T) {
	result, err := run(context.Background(), `
	let log = []
	function* gen() {
		yield 1
		log.append("after")
		yield 2
	}
	[gen(), log]
	`)
	if err != nil {
		t.Fatal(err)
	}
	list := result.(*object.List).Value()
	g := list[0].(*object.Generator)
	ctx := context.Background()
	mustPull(t, ctx, g)
	if _, err := g.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, done := genPull(t, context.Background(), g); !done {
		t.Fatal("done")
	}
}

func TestGeneratorCloseDelegatedChildFirst(t *testing.T) {
	result, err := run(context.Background(), `
	let log = []
	function* child() {
		try {
			yield 1
			yield 2
		} finally {
			log.append("child-finally")
		}
	}
	function* parent() {
		try {
			yield 0
			yield* child()
			yield 3
		} finally {
			log.append("parent-finally")
		}
	}
	[parent(), log]
	`)
	if err != nil {
		t.Fatal(err)
	}
	list := result.(*object.List).Value()
	parent := list[0].(*object.Generator)
	log := list[1].(*object.List)
	ctx := context.Background()
	// Pull the parent's first yield, then one delegated child item, then
	// close the parent while delegation is active.
	mustPull(t, ctx, parent)
	mustPull(t, ctx, parent)
	if _, err := parent.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := log.Inspect(); got != `["child-finally", "parent-finally"]` {
		t.Fatalf("delegate child must be closed before parent cleanup: %s", got)
	}
}

func TestGeneratorCloseReportsCleanupErrorOnce(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() {
		try {
			yield 1
		} finally {
			throw "cleanup-failed"
		}
	}
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	mustPull(t, ctx, g)
	err = genCloseErr(t, ctx, g)
	if !strings.Contains(err.Error(), "cleanup-failed") {
		t.Fatalf("close must report cleanup error, got %v", err)
	}
	// Object is terminal: cleanup is neither re-run nor re-reported.
	if _, err := g.Close(ctx); err != nil {
		t.Fatalf("second close must not re-report: %v", err)
	}
	if _, done := genPull(t, ctx, g); !done {
		t.Fatal("terminal after cleanup error")
	}
}

func genCloseErr(t *testing.T, ctx context.Context, g *object.Generator) error {
	t.Helper()
	if _, err := g.Close(ctx); err == nil {
		t.Fatal("expected close() error")
		return nil
	} else {
		return err
	}
}

func TestGeneratorMethodValues(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() { yield 1 }
	let g = gen()
	let n = g.next
	let c = g.close
	[n(), c()]
	`)
	if err != nil {
		t.Fatal(err)
	}
	items := result.(*object.List).Value()
	m, ok := items[0].(*object.Map)
	if !ok {
		t.Fatalf("next bound method result = %T", items[0])
	}
	v, _ := m.GetItem(object.NewString("value"))
	if asInt(t, v) != 1 {
		t.Fatal("bound next() must drive the generator")
	}
}

func TestGeneratorBareYield(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() {
		yield
		yield 2
	}
	gen()
	`)
	if err != nil {
		t.Fatal(err)
	}
	g := result.(*object.Generator)
	ctx := context.Background()
	v, done := genPull(t, ctx, g)
	if done || v == nil {
		t.Fatal("bare yield must yield nil")
	}
	if _, isNil := v.(*object.NilType); !isNil {
		t.Fatalf("bare yield value = %s, want nil", v.Inspect())
	}
	if asInt(t, mustPull(t, ctx, g)) != 2 {
		t.Fatal("yield after bare yield")
	}
}

func TestGeneratorRejectsYieldOutsideFunction(t *testing.T) {
	_, err := run(context.Background(), `yield 1`)
	if err == nil || !strings.Contains(err.Error(), "generator") {
		t.Fatalf("expected compile error for top-level yield, got %v", err)
	}
	_, err = run(context.Background(), `
	function ordinary() { return 1 }
	function* gen() {
		let f = function () { yield 1 }
		f()
		yield 2
	}
	gen()
	`)
	if err == nil || !strings.Contains(err.Error(), "generator") {
		t.Fatalf("yield in a nested ordinary function must fail, got %v", err)
	}
}

func TestGeneratorYieldInFinallyRejected(t *testing.T) {
	_, err := run(context.Background(), `
	function* gen() {
		try { yield 1 } finally { yield 2 }
	}
	gen()
	`)
	if err == nil || !strings.Contains(err.Error(), "finally") {
		t.Fatalf("yield in finally should be a compile error, got %v", err)
	}
}

func TestGeneratorScriptNextResultIsMap(t *testing.T) {
	result, err := run(context.Background(), `
	function* gen() { yield "ok" }
	let g = gen()
	let r = g.next()
	[r.value, r.done, r]
	`)
	if err != nil {
		t.Fatal(err)
	}
	items := result.(*object.List).Value()
	if asString(t, items[0]) != "ok" {
		t.Fatal("r.value")
	}
	if b, ok := items[1].(*object.Bool); !ok || b.Value() {
		t.Fatal("r.done must be false")
	}
	if _, ok := items[2].(*object.Map); !ok {
		t.Fatal("third item must be the map itself")
	}
}
