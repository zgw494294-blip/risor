package vm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/deepnoodle-ai/risor/v2/pkg/object"
)

// errYielded is the internal control signal used by the eval loop when a
// generator suspends at a yield. It never escapes the VM package.
var errYielded = errors.New("generator yielded")

// Compile-time assertion that the VM drives object.Generator instances via
// the public object interface.
var _ object.GeneratorDriver = (*VirtualMachine)(nil)

// genSusp is the engine-owned state of a suspended generator. It is stored
// opaquely on the object.Generator so that multiple generator instances can
// hold independent, interleaved suspensions on a single virtual machine.
type genSusp struct {
	// ip is the instruction pointer in the active (deepest) frame at which
	// execution resumes.
	ip int

	// stack holds the saved value-stack contents belonging to the generator.
	stack []object.Object

	// frames holds saved call frames, outermost first. Locals are always
	// backed by heap-allocated slices, never by the frame's inline storage.
	frames []frame

	// exc holds saved exception handlers, with fp relative to the
	// generator's outermost frame (0-based).
	exc []exceptionFrame

	// resumeValue, when non-nil, is pushed onto the value stack immediately
	// after resuming (the value of the yield expression).
	resumeValue object.Object

	// delegate is the active yield* delegate, if the generator is suspended
	// inside a YieldFrom instruction at delegateSite.
	delegate genDelegate

	// delegateSite is the IP of the YieldFrom instruction driving delegate.
	delegateSite int
}

// genDelegate is a pull-based iterator used to implement yield*.
type genDelegate interface {
	// next returns the next delegated value. done=true means the delegate is
	// exhausted. A finite delegate MUST eventually report done=true.
	next(ctx context.Context) (value object.Object, done bool, err error)

	// close stops the delegate early, running any cleanup it owns. It is
	// called before the delegating generator runs its own cleanup.
	close(ctx context.Context) error
}

// scriptDelegate delegates yield* to another script generator.
type scriptDelegate struct {
	vm *VirtualMachine
	g  *object.Generator
}

func (d *scriptDelegate) next(ctx context.Context) (object.Object, bool, error) {
	result, err := d.vm.driveGenerator(ctx, d.g, false)
	if err != nil {
		return nil, false, err
	}
	value, done, err := readGeneratorResult(result)
	if err != nil {
		return nil, false, err
	}
	return value, done, nil
}

func (d *scriptDelegate) close(ctx context.Context) error {
	_, err := d.vm.driveGenerator(ctx, d.g, true)
	return err
}

// listDelegate pulls items from a list without collecting them upfront.
type listDelegate struct {
	l *object.List
	i int
}

func (d *listDelegate) next(_ context.Context) (object.Object, bool, error) {
	items := d.l.Value()
	if d.i >= len(items) {
		return nil, true, nil
	}
	value := items[d.i]
	d.i++
	return value, false, nil
}

func (d *listDelegate) close(_ context.Context) error { return nil }

// rangeDelegate lazily walks a range.
type rangeDelegate struct {
	cur  int64
	stop int64
	step int64
}

func (d *rangeDelegate) next(_ context.Context) (object.Object, bool, error) {
	if d.step > 0 {
		if d.cur >= d.stop {
			return nil, true, nil
		}
	} else {
		if d.cur <= d.stop {
			return nil, true, nil
		}
	}
	value := object.NewInt(d.cur)
	d.cur += d.step
	return value, false, nil
}

func (d *rangeDelegate) close(_ context.Context) error { return nil }

// mapDelegate yields map keys in the map's stable sorted order, matching
// spread/unpack semantics. Keys snapshotted when delegation starts; values
// would be observed on each pull via the map's key/value iterators
// (object.NewMapValueIter and friends).
type mapDelegate struct {
	m    *object.Map
	keys []string
	i    int
}

func (d *mapDelegate) next(_ context.Context) (object.Object, bool, error) {
	if d.i >= len(d.keys) {
		return nil, true, nil
	}
	key := object.NewString(d.keys[d.i])
	d.i++
	return key, false, nil
}

func (d *mapDelegate) close(_ context.Context) error { return nil }

// bridgeItem is one message produced by the enumeration bridge goroutine.
type bridgeItem struct {
	value object.Object
	done  bool
}

// enumBridge adapts the push-based object.Enumerable protocol (used by host
// object.NewIter and other enumerables that do not have native pull support)
// to the pull-based delegate protocol. The producer runs on a goroutine that
// parks between items, so items are produced lazily one at a time instead of
// being collected upfront. The bridge goroutine exits on exhaustion, context
// cancellation, or close().
type enumBridge struct {
	en  object.Enumerable
	ctx context.Context

	itemCh chan bridgeItem // buffered(1): one produced item
	demand chan struct{}   // consumer asks for the next item
	stop   chan struct{}   // closed to release the producer early

	startOnce sync.Once
	stopOnce  sync.Once
	started   bool // consumer has received at least one item/done
	awaitNext bool // consumer owes a demand signal before the next item
}

func newEnumBridge(ctx context.Context, en object.Enumerable) *enumBridge {
	return &enumBridge{
		en:     en,
		ctx:    ctx,
		itemCh: make(chan bridgeItem, 1),
		demand: make(chan struct{}),
		stop:   make(chan struct{}),
	}
}

func (b *enumBridge) run() {
	go func() {
		b.en.Enumerate(b.ctx, func(_, value object.Object) bool {
			select {
			case b.itemCh <- bridgeItem{value: value}:
			case <-b.stop:
				return false
			case <-b.ctx.Done():
				return false
			}
			select {
			case <-b.demand:
				return true
			case <-b.stop:
				return false
			case <-b.ctx.Done():
				return false
			}
		})
		select {
		case b.itemCh <- bridgeItem{done: true}:
		case <-b.stop:
		case <-b.ctx.Done():
		}
	}()
}

func (b *enumBridge) next(ctx context.Context) (object.Object, bool, error) {
	if b.awaitNext {
		select {
		case b.demand <- struct{}{}:
		case <-b.stop:
			return nil, true, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		b.awaitNext = false
	}
	b.startOnce.Do(b.run)
	select {
	case item := <-b.itemCh:
		if item.done {
			return nil, true, nil
		}
		b.started = true
		b.awaitNext = true
		return item.value, false, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func (b *enumBridge) close(_ context.Context) error {
	b.stopOnce.Do(func() { close(b.stop) })
	return nil
}

// makeDelegate converts an object into a pull-based yield* delegate.
func (vm *VirtualMachine) makeDelegate(obj object.Object, userCtx context.Context) (genDelegate, error) {
	switch o := obj.(type) {
	case *object.Generator:
		return &scriptDelegate{vm: vm, g: o}, nil
	case *object.List:
		return &listDelegate{l: o}, nil
	case *object.Range:
		return &rangeDelegate{cur: o.Start(), stop: o.Stop(), step: o.Step()}, nil
	case *object.Map:
		return &mapDelegate{m: o, keys: o.SortedKeys()}, nil
	case object.Enumerable:
		// Host NewIter and other enumerables are lazily bridged so their
		// producer is never run ahead of consumption.
		return newEnumBridge(userCtx, o), nil
	default:
		return nil, vm.typeError("yield* requires an enumerable or generator (got %s)", obj.Type())
	}
}

// readGeneratorResult extracts (value, done) from a generator result map.
func readGeneratorResult(result object.Object) (object.Object, bool, error) {
	m, ok := result.(*object.Map)
	if !ok {
		return nil, false, errors.New("internal error: generator did not return a map")
	}
	doneObj, err := m.GetItem(object.NewString(object.GeneratorDoneKey))
	if err != nil {
		return nil, false, err.Value()
	}
	done, ok := doneObj.(*object.Bool)
	if !ok {
		return nil, false, errors.New("internal error: generator result has non-bool done")
	}
	value, err := m.GetItem(object.NewString(object.GeneratorValueKey))
	if err != nil {
		return nil, false, err.Value()
	}
	return value, done.Value(), nil
}

// makeGenerator validates call arguments and creates a generator object
// without executing the generator function body.
func (vm *VirtualMachine) makeGenerator(fn *object.Closure, args []object.Object) (*object.Generator, error) {
	if err := checkCallArgs(fn, len(args)); err != nil {
		return nil, err
	}
	argsCopy := make([]object.Object, len(args))
	copy(argsCopy, args)
	return object.NewGenerator(fn, argsCopy, vm), nil
}

// generatorDoneMap is the stable {value: null, done: true} result used after
// natural completion, termination by error, and close().
func generatorDoneMap() *object.Map {
	return object.NewGeneratorResult(object.Nil, true)
}

// GeneratorResume implements object.GeneratorDriver. It runs the generator
// body until the next yield or termination and returns the script-visible
// {value, done} map.
func (vm *VirtualMachine) GeneratorResume(ctx context.Context, g *object.Generator) (object.Object, error) {
	return vm.driveGenerator(ctx, g, false)
}

// GeneratorClose implements object.GeneratorDriver.
func (vm *VirtualMachine) GeneratorClose(ctx context.Context, g *object.Generator) (object.Object, error) {
	return vm.driveGenerator(ctx, g, true)
}

// driveGenerator is the shared implementation of next() and close().
func (vm *VirtualMachine) driveGenerator(userCtx context.Context, g *object.Generator, closing bool) (result object.Object, err error) {
	switch g.Status() {
	case object.GenRunning:
		return nil, errors.New("eval error: generator is already running")
	case object.GenClosed:
		if closing {
			return object.Nil, nil
		}
		if done := g.DoneResult(); done != nil {
			return done, nil
		}
		return generatorDoneMap(), nil
	case object.GenNew:
		if closing {
			// Never started: the body (including any finally block) never runs.
			g.SetStatus(object.GenClosed)
			g.SetDoneResult(generatorDoneMap())
			return object.Nil, nil
		}
	}

	// Enter the VM. When called from script code the VM is already running.
	// When called by an embedder (or after Eval returned), install a local
	// cancellation watcher that is guaranteed to have exited before this
	// call returns: repeated pulls on one VM must never let a stale watcher
	// flip the halt flag of a later pull.
	wasRunning := vm.running
	evalCtx := userCtx
	if !wasRunning {
		atomic.StoreInt32(&vm.halt, 0)
		watcherQuit := make(chan struct{})
		watcherDone := make(chan struct{})
		go func() {
			defer close(watcherDone)
			select {
			case <-userCtx.Done():
				atomic.StoreInt32(&vm.halt, 1)
			case <-watcherQuit:
			}
		}()
		defer func() {
			close(watcherQuit)
			<-watcherDone
		}()
	}
	evalCtx = vm.initContext(evalCtx)

	baseFP, baseSP, baseIP := vm.fp, vm.sp, vm.ip
	baseExc := vm.excStackSize
	prevGen := vm.activeGen
	prevUserCtx := vm.activeGenUserCtx
	prevGenBaseFP := vm.genBaseFP
	prevGenBaseSP := vm.genBaseSP
	prevGenBaseExc := vm.genBaseExc

	defer func() {
		if r := recover(); r != nil {
			if vm.panicStack == nil {
				vm.panicStack = vm.captureStack()
			}
			vm.detachGeneratorState(baseFP, baseSP, baseIP, baseExc)
			vm.activeGen = prevGen
			vm.activeGenUserCtx = prevUserCtx
			vm.genBaseFP = prevGenBaseFP
			vm.genBaseSP = prevGenBaseSP
			vm.genBaseExc = prevGenBaseExc
			g.SetStatus(object.GenClosed)
			g.SetDoneResult(generatorDoneMap())
			if wasRunning {
				panic(r) // let the outer eval/Run recover and format it
			}
			result = nil
			err = vm.panicToError(r)
			return
		}
		vm.detachGeneratorState(baseFP, baseSP, baseIP, baseExc)
		vm.activeGen = prevGen
		vm.activeGenUserCtx = prevUserCtx
		vm.genBaseFP = prevGenBaseFP
		vm.genBaseSP = prevGenBaseSP
		vm.genBaseExc = prevGenBaseExc
	}()

	vm.activeGen = g
	vm.activeGenUserCtx = userCtx
	vm.genBaseFP = baseFP
	vm.genBaseSP = baseSP
	vm.genBaseExc = baseExc

	startStatus := g.Status()
	g.SetStatus(object.GenRunning)

	if startStatus == object.GenNew {
		if err := vm.activateNewGeneratorFrame(g, baseFP); err != nil {
			g.SetStatus(object.GenClosed)
			g.SetDoneResult(generatorDoneMap())
			return nil, err
		}
	} else {
		susp := g.Suspension().(*genSusp)
		if err := vm.ensureFrameCapacity(baseFP + len(susp.frames)); err != nil {
			g.SetStatus(object.GenClosed)
			g.SetDoneResult(generatorDoneMap())
			return nil, err
		}
		vm.restoreGeneratorState(g, baseFP, baseSP)
	}

	// For close() on a started, suspended generator: close the active
	// delegate first, then finish active finally blocks without running any
	// business statements past the yield point.
	var pendingDelegateCloseErr error
	if closing {
		if susp, _ := g.Suspension().(*genSusp); susp != nil && susp.delegate != nil {
			pendingDelegateCloseErr = susp.delegate.close(userCtx)
			susp.delegate = nil
		}
		if !vm.enterGeneratorClose() {
			// No active finally block: there is nothing to run.
			g.SetStatus(object.GenClosed)
			g.SetDoneResult(generatorDoneMap())
			if pendingDelegateCloseErr != nil {
				return nil, pendingDelegateCloseErr
			}
			return object.Nil, nil
		}
	}

	evalErr := vm.eval(evalCtx)
	if evalErr == nil && pendingDelegateCloseErr != nil {
		evalErr = pendingDelegateCloseErr
	}

	if evalErr == errYielded {
		if closing {
			// Defensive: yield during close is impossible (yield is rejected
			// at compile time in finally blocks); do not leave it running.
			g.SetStatus(object.GenClosed)
			g.SetDoneResult(generatorDoneMap())
			return nil, errors.New("eval error: yield during generator close")
		}
		g.SetStatus(object.GenSuspended)
		return object.NewGeneratorResult(vm.yieldedValue, false), nil
	}

	if evalErr != nil {
		// Unhandled error (including errors from cleanup): reported by this
		// call; the generator is terminal and later next() calls return done.
		g.SetStatus(object.GenClosed)
		g.SetDoneResult(generatorDoneMap())
		return nil, evalErr
	}

	// Natural completion or explicit return: the frame's return value is on
	// the top of the generator's stack.
	returnValue := vm.pop()
	g.SetStatus(object.GenClosed)
	if closing {
		g.SetDoneResult(generatorDoneMap())
		return object.Nil, nil
	}
	doneResult := object.NewGeneratorResult(returnValue, true)
	g.SetDoneResult(doneResult)
	return doneResult, nil
}

// activateNewGeneratorFrame assembles parameters/defaults/rest and activates
// the generator's outermost frame without running any body instructions.
func (vm *VirtualMachine) activateNewGeneratorFrame(g *object.Generator, baseFP int) error {
	fn := g.Func()
	localValues, argc := vm.assembleLocals(fn, g.Args())
	if _, err := vm.activateFunction(baseFP+1, 0, fn, localValues[:argc]); err != nil {
		return err
	}
	vm.activeFrame.returnAddr = StopSignal

	if vm.observer != nil && vm.observerConfig.ObserveCalls {
		event := CallEvent{
			FunctionName: fn.Name(),
			ArgCount:     len(g.Args()),
			Location:     vm.getCurrentLocation(),
			FrameDepth:   vm.fp + 1,
		}
		if !vm.observer.OnCall(event) {
			return errors.New("execution halted by observer")
		}
	}
	return nil
}

// enterGeneratorClose simulates a "return nil" at the current suspension
// point: if an enclosing finally block is active, arrange for it to run. It
// returns false when there is no finally block to execute.
func (vm *VirtualMachine) enterGeneratorClose() bool {
	idx := vm.innermostFinallyIndex(vm.fp, vm.activeCode)
	if idx < 0 {
		return false
	}
	ef := &vm.excStack[idx]
	ef.pendingReturn = object.Nil
	ef.inFinally = true
	vm.ip = ef.handler.FinallyStart
	return true
}

// suspendYield captures the generator's frames, value stack and exception
// handlers into g and records the yielded value. resumeValue is pushed after
// resuming (nil for plain yield's expression result); pass nil interface for
// yield* suspensions, which re-enter the YieldFrom instruction instead.
func (vm *VirtualMachine) suspendYield(g *object.Generator, value object.Object, resumeValue object.Object) {
	susp, _ := g.Suspension().(*genSusp)
	if susp == nil {
		susp = &genSusp{}
	}

	// Capture call frames (outermost first). Inline frame storage is copied
	// to heap slices so the state survives the VM frame slots being reused
	// while the generator is suspended.
	frameCount := vm.fp - vm.genBaseFP
	susp.frames = make([]frame, frameCount)
	for i := 0; i < frameCount; i++ {
		src := &vm.frames[vm.genBaseFP+1+i]
		f := *src
		if f.extendedLocals == nil && f.capturedLocals == nil {
			heapLocals := make([]object.Object, len(f.locals))
			copy(heapLocals, f.locals)
			f.locals = heapLocals
		}
		f.returnSp -= vm.genBaseSP
		susp.frames[i] = f
	}

	// Capture value stack contents.
	stackCount := vm.sp - vm.genBaseSP
	if stackCount > 0 {
		susp.stack = make([]object.Object, stackCount)
		copy(susp.stack, vm.stack[vm.genBaseSP+1:vm.sp+1])
	} else {
		susp.stack = nil
	}

	// Capture exception handlers, rebasing frame pointers to be relative.
	excCount := vm.excStackSize - vm.genBaseExc
	if excCount > 0 {
		susp.exc = make([]exceptionFrame, excCount)
		copy(susp.exc, vm.excStack[vm.genBaseExc:vm.excStackSize])
		for i := range susp.exc {
			susp.exc[i].fp -= vm.genBaseFP + 1
		}
	} else {
		susp.exc = nil
	}

	susp.ip = vm.ip
	susp.resumeValue = resumeValue
	g.SetSuspension(susp)
	vm.yieldedValue = value
}

// restoreGeneratorState copies a suspended generator's state back onto the
// VM above the current caller boundary.
func (vm *VirtualMachine) restoreGeneratorState(g *object.Generator, baseFP, baseSP int) {
	susp := g.Suspension().(*genSusp)

	for i, saved := range susp.frames {
		slot := baseFP + 1 + i
		vm.frames[slot] = saved
		f := &vm.frames[slot]
		switch {
		case f.capturedLocals != nil:
			f.locals = f.capturedLocals
		case f.extendedLocals != nil:
			f.locals = f.extendedLocals
		}
		f.returnSp = saved.returnSp + baseSP
	}

	for i, value := range susp.stack {
		vm.stack[baseSP+1+i] = value
	}
	vm.sp = baseSP + len(susp.stack)

	vm.fp = baseFP + len(susp.frames)
	for _, saved := range susp.exc {
		vm.ensureExcCapacity()
		ef := saved
		ef.fp += baseFP + 1
		vm.excStack[vm.excStackSize] = ef
		vm.excStackSize++
	}

	vm.activeFrame = &vm.frames[vm.fp]
	vm.activeCode = vm.activeFrame.code
	vm.ip = susp.ip

	if susp.resumeValue != nil {
		vm.push(susp.resumeValue)
		susp.resumeValue = nil
	}
}

// detachGeneratorState removes all VM state belonging to a generator drive
// and restores the caller's frame, instruction pointer and stack pointers.
// It runs on every exit from driveGenerator: yield, completion, error and
// panic.
func (vm *VirtualMachine) detachGeneratorState(baseFP, baseSP, baseIP, baseExc int) {
	// Clear value stack contents above the caller boundary.
	for i := vm.sp; i > baseSP; i-- {
		vm.stack[i] = nil
	}
	// Clear frame slots belonging to the generator.
	for fp := vm.fp; fp > baseFP; fp-- {
		vm.frames[fp] = frame{}
	}
	// Clear exception handlers belonging to the generator.
	for i := vm.excStackSize - 1; i >= baseExc; i-- {
		vm.excStack[i] = exceptionFrame{}
	}
	vm.excStackSize = baseExc
	// Resume the caller frame.
	vm.resumeFrame(baseFP, baseIP, baseSP)
}

// innermostFinallyIndex returns the index of the topmost active exception
// handler for the given frame/code that owns a finally block which has not
// run yet. Returns -1 when there is none.
func (vm *VirtualMachine) innermostFinallyIndex(fp int, code *loadedCode) int {
	for i := vm.excStackSize - 1; i >= 0; i-- {
		ef := &vm.excStack[i]
		if ef.fp != fp || ef.code != code {
			break
		}
		if ef.handler.FinallyStart > 0 && !ef.inFinally {
			return i
		}
	}
	return -1
}

// popHandlersForFrame removes exception handlers belonging to fp/code from
// the exception stack. Used when a frame returns through finally blocks,
// since intervening catch-only handlers no longer have work to do.
func (vm *VirtualMachine) popHandlersForFrame(fp int, code *loadedCode) {
	for vm.excStackSize > 0 {
		ef := &vm.excStack[vm.excStackSize-1]
		if ef.fp != fp || ef.code != code {
			break
		}
		vm.excStack[vm.excStackSize-1] = exceptionFrame{}
		vm.excStackSize--
	}
}

// returnFromActiveFrame pops the active frame and notifies the observer.
// It returns stop=true when the eval loop must stop because the frame's
// return address is StopSignal.
func (vm *VirtualMachine) returnFromActiveFrame() (stop bool, err error) {
	activeFrame := vm.activeFrame
	// Any exception handlers still belonging to the frame being popped are
	// now stale.
	vm.popHandlersForFrame(vm.fp, vm.activeCode)
	if vm.observer != nil && vm.observerConfig.ObserveReturns {
		funcName := ""
		if activeFrame.fn != nil {
			funcName = activeFrame.fn.Name()
		}
		event := ReturnEvent{
			FunctionName: funcName,
			Location:     vm.getCurrentLocation(),
			FrameDepth:   vm.fp,
		}
		if !vm.observer.OnReturn(event) {
			return false, errors.New("execution halted by observer")
		}
	}
	returnAddr := activeFrame.returnAddr
	returnSp := activeFrame.returnSp
	returnFp := vm.fp - 1
	vm.resumeFrame(returnFp, returnAddr, returnSp)
	return returnAddr == StopSignal, nil
}

// ensureExcCapacity grows the exception stack when needed.
func (vm *VirtualMachine) ensureExcCapacity() {
	if vm.excStackSize < len(vm.excStack) {
		return
	}
	newCap := len(vm.excStack) * 2
	if newCap == 0 {
		newCap = 8
	}
	newStack := make([]exceptionFrame, newCap)
	copy(newStack, vm.excStack)
	vm.excStack = newStack
}
