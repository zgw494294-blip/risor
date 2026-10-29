package object

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/risor/v2/pkg/op"
)

// GENERATOR type constant
const GENERATOR Type = "generator"

// Generator lifecycle statuses.
const (
	// GenNew means the generator was created but next() has not run the body.
	GenNew = iota
	// GenSuspended means the generator is paused at a yield and can be resumed.
	GenSuspended
	// GenRunning means the generator body is currently executing. A
	// synchronous generator is only observed in this state while its own
	// code runs; re-entering next()/close() then is an error.
	GenRunning
	// GenClosed is terminal: the generator finished, returned, was closed,
	// or terminated by an unhandled error.
	GenClosed
)

// GeneratorResultKey names the fields in the map returned by next().
const (
	GeneratorValueKey = "value"
	GeneratorDoneKey  = "done"
)

// GeneratorDriver is implemented by the execution engine that knows how to
// run and suspend a generator's compiled body. The driver is bound to the
// virtual machine that created the generator, so generators remain usable
// (via GetAttr/Callable.Call) after the originating Eval/Run returns.
type GeneratorDriver interface {
	// GeneratorResume runs the body until the next yield or completion.
	// It returns the same {value, done} map that script code sees.
	GeneratorResume(ctx context.Context, g *Generator) (Object, error)
	// GeneratorClose requests early termination: delegated children are
	// closed first, then active finally blocks run, but business code after
	// the suspension point is skipped.
	GeneratorClose(ctx context.Context, g *Generator) (Object, error)
}

// Generator is a synchronous generator instance produced by calling a
// generator function ("function*"). The body does not run until the first
// next() call. Generators are driven serially; concurrent next()/close()
// calls are not supported.
type Generator struct {
	fn     *Closure
	args   []Object
	driver GeneratorDriver

	status int
	susp   any // Engine-owned suspension state (opaque to this package)

	// doneResult is the terminal {value, done:true} map returned by next()
	// once the generator is closed. Subsequent next() calls keep returning it.
	doneResult Object

	nextMethod  Object
	closeMethod Object
}

var _ Object = (*Generator)(nil)

// NewGenerator creates a generator instance for the given closure and the
// arguments the generator function was called with. The body is not executed.
func NewGenerator(fn *Closure, args []Object, driver GeneratorDriver) *Generator {
	g := &Generator{fn: fn, args: args, driver: driver, status: GenNew}
	g.nextMethod = NewBuiltin("next", func(ctx context.Context, _ ...Object) (Object, error) {
		return g.Next(ctx)
	})
	g.closeMethod = NewBuiltin("close", func(ctx context.Context, _ ...Object) (Object, error) {
		return g.Close(ctx)
	})
	return g
}

// Func returns the generator function closure.
func (g *Generator) Func() *Closure { return g.fn }

// Args returns the arguments the generator function was called with.
// Default and rest parameters are applied when the body first starts.
func (g *Generator) Args() []Object { return g.args }

// Status returns one of the Gen* status constants.
func (g *Generator) Status() int { return g.status }

// SetStatus updates the lifecycle status. Used by the execution engine.
func (g *Generator) SetStatus(status int) { g.status = status }

// Suspension returns the engine-owned opaque suspension state, or nil.
func (g *Generator) Suspension() any { return g.susp }

// SetSuspension stores engine-owned opaque suspension state.
func (g *Generator) SetSuspension(susp any) { g.susp = susp }

// DoneResult returns the stable terminal result map, or nil if the generator
// is not closed yet.
func (g *Generator) DoneResult() Object { return g.doneResult }

// SetDoneResult stores the stable terminal result map. Used by the execution
// engine.
func (g *Generator) SetDoneResult(result Object) { g.doneResult = result }

// Next advances the generator to the next yielded value or completion.
// It returns a map with "value" and "done" keys. This is invoked by the
// script-exposed "next" builtin and may be called directly by embedders.
func (g *Generator) Next(ctx context.Context) (Object, error) {
	if g.driver == nil {
		return nil, fmt.Errorf("eval error: generator has no execution engine")
	}
	return g.driver.GeneratorResume(ctx, g)
}

// Close terminates the generator early. It closes any active delegate, runs
// pending finally blocks, and skips business statements after the suspension
// point. Closing a never-started or already terminal generator is a no-op
// that succeeds.
func (g *Generator) Close(ctx context.Context) (Object, error) {
	if g.driver == nil {
		return nil, fmt.Errorf("eval error: generator has no execution engine")
	}
	return g.driver.GeneratorClose(ctx, g)
}

func (g *Generator) Type() Type { return GENERATOR }

func (g *Generator) Inspect() string {
	if g.fn != nil && g.fn.Name() != "" {
		return fmt.Sprintf("generator(%s)", g.fn.Name())
	}
	return "generator"
}

func (g *Generator) String() string { return g.Inspect() }

func (g *Generator) Interface() interface{} { return nil }

func (g *Generator) Equals(other Object) bool {
	otherGen, ok := other.(*Generator)
	return ok && g == otherGen
}

func (g *Generator) Attrs() []AttrSpec {
	return []AttrSpec{
		{Name: "next", Doc: "Advance the generator to the next yielded value", Returns: "map"},
		{Name: "close", Doc: "Stop the generator and run pending finally blocks", Returns: "null"},
	}
}

func (g *Generator) GetAttr(name string) (Object, bool) {
	switch name {
	case "next":
		return g.nextMethod, true
	case "close":
		return g.closeMethod, true
	default:
		return nil, false
	}
}

func (g *Generator) SetAttr(name string, value Object) error {
	return TypeErrorf("generator has no attribute %q", name)
}

func (g *Generator) IsTruthy() bool { return true }

func (g *Generator) RunOperation(opType op.BinaryOpType, right Object) (Object, error) {
	return nil, newTypeErrorf("unsupported operation for generator: %v", opType)
}

func (g *Generator) MarshalJSON() ([]byte, error) {
	return nil, TypeErrorf("unable to marshal generator")
}

// NewGeneratorResult builds the result map returned by next(): a script map
// with "value" and boolean "done" fields.
func NewGeneratorResult(value Object, done bool) *Map {
	if value == nil {
		value = Nil
	}
	return NewMap(map[string]Object{
		GeneratorValueKey: value,
		GeneratorDoneKey:  NewBool(done),
	})
}
