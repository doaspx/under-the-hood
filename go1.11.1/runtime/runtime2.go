// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// 定义的敞亮
const (
	// G 的状态
	//
	// 除了指示 G 的一般状态之外，G 的状态就类似于 goroutine 的堆栈的一个锁（即它执行用户代码的能力）。
	// 如果你在给此列表增加内容，还需要添加到 mgcmark.go 中的 “垃圾收集期间的正常” 状态列表中。
	//

	// _Gidle 表示 goroutine 刚刚完成分配且还未被初始化
	_Gidle = iota // 0

	// _Grunnable 表示 goroutine 已经在运行队列中。当前还未运行用户代码。还未拥有运行栈。
	_Grunnable // 1

	// _Grunning 表示 goroutine 可能正在运行用户代码。运行栈被该 goroutine 拥有。
	// 该 goroutine 不在运行队列中，且被分配了一个 M 和 一个 P。
	_Grunning // 2

	// _Gsyscall 表示当前 goroutine 正在执行一个系统调用，没有执行用户代码。
	// 该 goroutine 拥有一个栈且不在运行队列中，并分配有一个 M
	_Gsyscall // 3

	// _Gwaiting 表示当前 goroutine 在运行时中被阻塞。它没有执行用户代码。它不在运行队列中，
	// 但应该记录在某处（比如一个 channel 等待队列）因此可以在需要时 ready()。
	// 除了 channel 操作可以在适当的 channel 锁定下读取或写入堆栈的部分之外，它不拥有堆栈。
	// 否则，在 goroutine 进入 _Gwaiting 后访问堆栈是不安全的（例如，它可能会被移动）。
	_Gwaiting // 4

	// _Gmoribund_unused is currently unused, but hardcoded in gdb
	// scripts.
	_Gmoribund_unused // 5

	// _Gdead 表示当前 goroutine 当前未被使用。它可能刚被执行、
	// 在释放列表中、或刚刚被初始化。它没有执行用户代码。它可能也可能没有分配的栈。
	// G 及其栈（如果有）由正在退出 G 或从释放列表获得 G 的 M 拥有
	_Gdead // 6

	// _Genqueue_unused 表示 G 当前未被使用
	_Genqueue_unused // 7

	// _Gcopystack means this goroutine's stack is being moved. It
	// is not executing user code and is not on a run queue. The
	// stack is owned by the goroutine that put it in _Gcopystack.
	_Gcopystack // 8

	// _Gscan combined with one of the above states other than
	// _Grunning indicates that GC is scanning the stack. The
	// goroutine is not executing user code and the stack is owned
	// by the goroutine that set the _Gscan bit.
	//
	// _Gscanrunning is different: it is used to briefly block
	// state transitions while GC signals the G to scan its own
	// stack. This is otherwise like _Grunning.
	//
	// atomicstatus&~Gscan gives the state the goroutine will
	// return to when the scan completes.
	_Gscan         = 0x1000
	_Gscanrunnable = _Gscan + _Grunnable // 0x1001
	_Gscanrunning  = _Gscan + _Grunning  // 0x1002
	_Gscansyscall  = _Gscan + _Gsyscall  // 0x1003
	_Gscanwaiting  = _Gscan + _Gwaiting  // 0x1004
)

const (
	// P 的状态
	_Pidle    = iota
	_Prunning // 只允许此 P 从 _Prunning 更改
	_Psyscall
	_Pgcstop
	_Pdead
)

// 互斥锁。在无竞争的情况下，与自旋锁 spin lock（只是一些用户级指令）一样快，
// 但在争用路径 contention path 中，它们在内核中休眠。零值互斥锁为未加锁状态（无需初始化每个锁）。
type mutex struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

// sleep and wakeup on one-time events.
// before any calls to notesleep or notewakeup,
// must call noteclear to initialize the Note.
// then, exactly one thread can call notesleep
// and exactly one thread can call notewakeup (once).
// once notewakeup has been called, the notesleep
// will return.  future notesleep will return immediately.
// subsequent noteclear must be called only after
// previous notesleep has returned, e.g. it's disallowed
// to call noteclear straight after notewakeup.
//
// notetsleep is like notesleep but wakes up after
// a given number of nanoseconds even if the event
// has not yet happened.  if a goroutine uses notetsleep to
// wake up early, it must wait to call noteclear until it
// can be sure that no other goroutine is calling
// notewakeup.
//
// notesleep/notetsleep are generally called on g0,
// notetsleepg is similar to notetsleep but is called on user g.
type note struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

type funcval struct {
	fn uintptr
	// 变长大小，fn 的数据在应在 fn 之后
}

type iface struct {
	tab  *itab
	data unsafe.Pointer
}

type eface struct {
	_type *_type
	data  unsafe.Pointer
}

func efaceOf(ep *interface{}) *eface {
	return (*eface)(unsafe.Pointer(ep))
}

// The guintptr, muintptr, and puintptr are all used to bypass write barriers.
// It is particularly important to avoid write barriers when the current P has
// been released, because the GC thinks the world is stopped, and an
// unexpected write barrier would not be synchronized with the GC,
// which can lead to a half-executed write barrier that has marked the object
// but not queued it. If the GC skips the object and completes before the
// queuing can occur, it will incorrectly free the object.
//
// We tried using special assignment functions invoked only when not
// holding a running P, but then some updates to a particular memory
// word went through write barriers and some did not. This breaks the
// write barrier shadow checking mode, and it is also scary: better to have
// a word that is completely ignored by the GC than to have one for which
// only a few updates are ignored.
//
// Gs and Ps are always reachable via true pointers in the
// allgs and allp lists or (during allocation before they reach those lists)
// from stack variables.
//
// Ms are always reachable via true pointers either from allm or
// freem. Unlike Gs and Ps we do free Ms, so it's important that
// nothing ever hold an muintptr across a safe point.

// A guintptr holds a goroutine pointer, but typed as a uintptr
// to bypass write barriers. It is used in the Gobuf goroutine state
// and in scheduling lists that are manipulated without a P.
//
// The Gobuf.g goroutine pointer is almost always updated by assembly code.
// In one of the few places it is updated by Go code - func save - it must be
// treated as a uintptr to avoid a write barrier being emitted at a bad time.
// Instead of figuring out how to emit the write barriers missing in the
// assembly manipulation, we change the type of the field to uintptr,
// so that it does not require write barriers at all.
//
// Goroutine structs are published in the allg list and never freed.
// That will keep the goroutine structs from being collected.
// There is never a time that Gobuf.g's contain the only references
// to a goroutine: the publishing of the goroutine in allg comes first.
// Goroutine pointers are also kept in non-GC-visible places like TLS,
// so I can't see them ever moving. If we did want to start moving data
// in the GC, we'd need to allocate the goroutine structs from an
// alternate arena. Using guintptr doesn't make that problem any worse.
type guintptr uintptr

//go:nosplit
func (gp guintptr) ptr() *g { return (*g)(unsafe.Pointer(gp)) }

//go:nosplit
func (gp *guintptr) set(g *g) { *gp = guintptr(unsafe.Pointer(g)) }

//go:nosplit
func (gp *guintptr) cas(old, new guintptr) bool {
	return atomic.Casuintptr((*uintptr)(unsafe.Pointer(gp)), uintptr(old), uintptr(new))
}

// setGNoWB performs *gp = new without a write barrier.
// For times when it's impractical to use a guintptr.
//go:nosplit
//go:nowritebarrier
func setGNoWB(gp **g, new *g) {
	(*guintptr)(unsafe.Pointer(gp)).set(new)
}

type puintptr uintptr

//go:nosplit
func (pp puintptr) ptr() *p { return (*p)(unsafe.Pointer(pp)) }

//go:nosplit
func (pp *puintptr) set(p *p) { *pp = puintptr(unsafe.Pointer(p)) }

// muintptr is a *m that is not tracked by the garbage collector.
//
// Because we do free Ms, there are some additional constrains on
// muintptrs:
//
// 1. Never hold an muintptr locally across a safe point.
//
// 2. Any muintptr in the heap must be owned by the M itself so it can
//    ensure it is not in use when the last true *m is released.
type muintptr uintptr

//go:nosplit
func (mp muintptr) ptr() *m { return (*m)(unsafe.Pointer(mp)) }

//go:nosplit
func (mp *muintptr) set(m *m) { *mp = muintptr(unsafe.Pointer(m)) }

// setMNoWB performs *mp = new without a write barrier.
// For times when it's impractical to use an muintptr.
//go:nosplit
//go:nowritebarrier
func setMNoWB(mp **m, new *m) {
	(*muintptr)(unsafe.Pointer(mp)).set(new)
}

type gobuf struct {
	// The offsets of sp, pc, and g are known to (hard-coded in) libmach.
	//
	// ctxt is unusual with respect to GC: it may be a
	// heap-allocated funcval, so GC needs to track it, but it
	// needs to be set and cleared from assembly, where it's
	// difficult to have write barriers. However, ctxt is really a
	// saved, live register, and we only ever exchange it between
	// the real register and the gobuf. Hence, we treat it as a
	// root during stack scanning, which means assembly that saves
	// and restores it doesn't need write barriers. It's still
	// typed as a pointer so that any other writes from Go get
	// write barriers.
	sp   uintptr
	pc   uintptr
	g    guintptr
	ctxt unsafe.Pointer
	ret  sys.Uintreg
	lr   uintptr
	bp   uintptr // for GOEXPERIMENT=framepointer
}

// sudog 表示了一个等待队列中的 g，例如在一个 channel 中进行发送和接受
//
// sudog 是必要的，因为 g <-> 同步对象之间的关系是多对多。一个 g 可以在多个等待列表上，
// 因此可以有很多的 sudog 为一个 g 服务；并且很多 g 可能在等待同一个同步对象，
// 因此也会有很多 sudog 为一个同步对象服务。
//
// 所有的 sudog 分配在一个特殊的池中。使用 acquireSudog 和 releaseSudog 来分配并释放它们。
type sudog struct {
	// 下面的字段由这个 sudog 阻塞的通道的 hchan.lock 进行保护。
	// shrinkstack 依赖于它服务于 sudog 相关的 channel 操作。

	g *g

	// isSelect 表示 g 正在参与一个 select，因此 g.selectDone 必须以 CAS 的方式来避免唤醒时候的 data race。
	isSelect bool
	next     *sudog
	prev     *sudog
	elem     unsafe.Pointer // 数据元素（可能指向栈）

	// 下面的字段永远不会并发的被访问。对于 channel waitlink 只会被 g 访问
	// 对于 semaphores，所有的字段（包括上面的）只会在持有 semaRoot 锁时被访问

	acquiretime int64
	releasetime int64
	ticket      uint32
	parent      *sudog // semaRoot 二叉树
	waitlink    *sudog // g.waiting 列表或 semaRoot
	waittail    *sudog // semaRoot
	c           *hchan // channel
}

type libcall struct {
	fn   uintptr
	n    uintptr // 参数的个数
	args uintptr // 参数
	r1   uintptr // 返回值
	r2   uintptr
	err  uintptr // 错误号
}

// 描述了如何处理回调
type wincallbackcontext struct {
	gobody       unsafe.Pointer // 要调度的 go 函数
	argsize      uintptr        // 回调参数大小（字节）
	restorestack uintptr        // adjust stack on return by (in bytes) (386 only)
	cleanstack   bool
}

// Stack 描述了 Go 的执行栈，栈的区间为 [lo, hi)，在栈两边没有任何隐式数据结构
// 因此 Go 的执行栈由运行时管理，本质上分配在堆中，比 ulimit -s 大
type stack struct {
	lo uintptr
	hi uintptr
}

type g struct {
	// Stack 参数
	// stack 描述了实际的栈内存：[stack.lo, stack.hi)
	stack stack // 偏移量与 runtime/cgo 一致
	// stackguard0 是对比 Go 栈增长的 prologue 的栈指针
	// 如果 sp 寄存器比 stackguard0 小（由于栈往低地址方向增长），会触发栈拷贝和调度
	// 通常情况下：stackguard0 = stack.lo + StackGuard，但被抢占时会变为 StackPreempt
	stackguard0 uintptr // 偏移量与 liblink 一致
	// stackguard1 是对比 C 栈增长的 prologue 的栈指针
	// 当位于 g0 和 gsignal 栈上时，值为 stack.lo + StackGuard
	// 在其他栈上值为 ~0 用于触发 morestackc (并 crash) 调用
	stackguard1 uintptr // 偏移量与 liblink 一致

	_panic         *_panic // innermost panic - offset known to liblink
	_defer         *_defer // innermost defer
	m              *m      // 当前的 m; 偏移量对 arm liblink 透明
	sched          gobuf
	syscallsp      uintptr        // if status==Gsyscall, syscallsp = sched.sp to use during gc
	syscallpc      uintptr        // if status==Gsyscall, syscallpc = sched.pc to use during gc
	stktopsp       uintptr        // expected sp at top of stack, to check in traceback
	param          unsafe.Pointer // passed parameter on wakeup
	atomicstatus   uint32
	stackLock      uint32 // sigprof/scang lock; TODO: fold in to atomicstatus
	goid           int64
	schedlink      guintptr
	waitsince      int64      // approx time when the g become blocked
	waitreason     waitReason // if status==Gwaiting
	preempt        bool       // preemption signal, duplicates stackguard0 = stackpreempt
	paniconfault   bool       // panic (instead of crash) on unexpected fault address
	preemptscan    bool       // preempted g does scan for gc
	gcscandone     bool       // g has scanned stack; protected by _Gscan bit in status
	gcscanvalid    bool       // 在 gc 周期开始时为 false；当 G 从上次 scan 后就没有运行时为 true TODO: remove?
	throwsplit     bool       // must not split stack
	raceignore     int8       // ignore race detection events
	sysblocktraced bool       // StartTrace has emitted EvGoInSyscall about this goroutine
	sysexitticks   int64      // cputicks when syscall has returned (for tracing)
	traceseq       uint64     // trace event sequencer
	tracelastp     puintptr   // last P emitted an event for this goroutine
	lockedm        muintptr
	sig            uint32
	writebuf       []byte
	sigcode0       uintptr
	sigcode1       uintptr
	sigpc          uintptr
	gopc           uintptr         // 当前创建 goroutine go 语句的 pc 寄存器
	ancestors      *[]ancestorInfo // ancestor information goroutine(s) that created this goroutine (only used if debug.tracebackancestors)
	startpc        uintptr         // goroutine 函数的 pc 寄存器
	racectx        uintptr
	waiting        *sudog         // sudog structures this g is waiting on (that have a valid elem ptr); in lock order
	cgoCtxt        []uintptr      // cgo traceback context
	labels         unsafe.Pointer // profiler 的标签
	timer          *timer         // 为 time.Sleep 缓存的计时器
	selectDone     uint32         // are we participating in a select and did someone win the race?

	// Per-G GC 状态

	// gcAssistBytes is this G's GC assist credit in terms of
	// bytes allocated. If this is positive, then the G has credit
	// to allocate gcAssistBytes bytes without assisting. If this
	// is negative, then the G must correct this by performing
	// scan work. We track this in bytes to make it fast to update
	// and check for debt in the malloc hot path. The assist ratio
	// determines how this corresponds to scan work debt.
	gcAssistBytes int64
}

type m struct {
	g0      *g     // 用于执行调度指令的 goroutine
	morebuf gobuf  // morestack 的 gobuf 参数
	divmod  uint32 // div/mod denominator for arm - known to liblink

	// debugger 不知道的字段
	procid        uint64       // 用于 debugger，偏移量不是写死的
	gsignal       *g           // 处理 signal 的 g
	goSigStack    gsignalStack // Go 分配的 signal handling 栈
	sigmask       sigset       // 用于保存 saved signal mask
	tls           [6]uintptr   // thread-local storage (对 x86 而言为额外的寄存器)
	mstartfn      func()
	curg          *g       // 当前运行的用户 goroutine
	caughtsig     guintptr // goroutine 在 fatal signal 中运行
	p             puintptr // attached p for executing go code (nil if not executing go code)
	nextp         puintptr
	id            int64
	mallocing     int32
	throwing      int32
	preemptoff    string // 如果不为空串 ""，继续让当前 g 运行在该 M 上
	locks         int32
	dying         int32
	profilehz     int32
	helpgc        int32
	spinning      bool // m 当前没有运行 work 且正处于寻找 work 的活跃状态
	blocked       bool // m 阻塞在一个 note 上
	inwb          bool // m 正在执行 write barrier
	newSigstack   bool // C 线程上的 minit 调用了 signalstack（C 调用 Go?）
	printlock     int8
	incgo         bool   // m 正在执行 cgo 调用
	freeWait      uint32 // if == 0, safe to free g0 and delete m (atomic)
	fastrand      [2]uint32
	needextram    bool
	traceback     uint8
	ncgocall      uint64      // 总共的 cgo 调用数
	ncgo          int32       // 正在进行的 cgo 调用数
	cgoCallersUse uint32      // 如果非零，则表示 cgoCaller 正在临时使用
	cgoCallers    *cgoCallers // cgo 调用崩溃的 cgo 回溯
	park          note
	alllink       *m // 在 allm 上
	schedlink     muintptr
	mcache        *mcache
	lockedg       guintptr
	createstack   [32]uintptr    // 当前线程创建的栈
	lockedExt     uint32         // 外部 LockOSThread 追踪
	lockedInt     uint32         // 内部 lockOSThread 追踪
	nextwaitm     muintptr       // 正在等待锁的下一个 m
	waitunlockf   unsafe.Pointer // todo go func(*g, unsafe.pointer) bool
	waitlock      unsafe.Pointer
	waittraceev   byte
	waittraceskip int
	startingtrace bool
	syscalltick   uint32
	thread        uintptr // 线程处理
	freelink      *m      // 在 sched.freem 上

	// 下面这些字段因为它们太大而不能放在低级的 NOSPLIT 函数的堆栈上。
	libcall   libcall
	libcallpc uintptr // 用于 cpu profiler
	libcallsp uintptr
	libcallg  guintptr
	syscall   libcall // 存储 windows 上系统调用的参数

	vdsoSP uintptr // SP 用于 VDSO 调用的回溯 (如果没有产生调用则为 0)
	vdsoPC uintptr // PC 用于 VDSO 调用的回溯

	mOS
}

type p struct {
	lock mutex

	id          int32
	status      uint32 // p 的状态 pidle/prunning/...
	link        puintptr
	schedtick   uint32     // 每次调度器调用都会增加
	syscalltick uint32     // 每次进行系统调用都会增加
	sysmontick  sysmontick // 系统监控观察到的最后一次记录
	m           muintptr   // 反向链接到关联的 m （nil 则表示 idle）
	mcache      *mcache
	racectx     uintptr

	deferpool    [5][]*_defer // 不同大小的可用的 defer 结构池 (见 panic.go)
	deferpoolbuf [5][32]*_defer

	// goroutine id 的缓存，用于均摊 runtime·sched.goidgen 的访问
	goidcache    uint64
	goidcacheend uint64

	// Queue of runnable goroutines. Accessed without lock.
	runqhead uint32
	runqtail uint32
	runq     [256]guintptr
	// runnext, if non-nil, is a runnable G that was ready'd by
	// the current G and should be run next instead of what's in
	// runq if there's time remaining in the running G's time
	// slice. It will inherit the time left in the current time
	// slice. If a set of goroutines is locked in a
	// communicate-and-wait pattern, this schedules that set as a
	// unit and eliminates the (potentially large) scheduling
	// latency that otherwise arises from adding the ready'd
	// goroutines to the end of the run queue.
	runnext guintptr

	// 有效的 G (状态 == Gdead)
	gfree    *g
	gfreecnt int32

	sudogcache []*sudog
	sudogbuf   [128]*sudog

	tracebuf traceBufPtr

	// traceSweep indicates the sweep events should be traced.
	// This is used to defer the sweep start event until a span
	// has actually been swept.
	traceSweep bool
	// traceSwept and traceReclaimed track the number of bytes
	// swept and reclaimed by sweeping in the current sweep loop.
	traceSwept, traceReclaimed uintptr

	palloc persistentAlloc // per-P to avoid mutex

	// Per-P GC state
	gcAssistTime         int64 // Nanoseconds in assistAlloc
	gcFractionalMarkTime int64 // Nanoseconds in fractional mark worker
	gcBgMarkWorker       guintptr
	gcMarkWorkerMode     gcMarkWorkerMode

	// gcMarkWorkerStartTime is the nanotime() at which this mark
	// worker started.
	gcMarkWorkerStartTime int64

	// gcw is this P's GC work buffer cache. The work buffer is
	// filled by write barriers, drained by mutator assists, and
	// disposed on certain GC state transitions.
	gcw gcWork

	// wbBuf 是当前 P 的 GC 的 write barrier 缓存
	//
	// TODO: Consider caching this in the running G.
	wbBuf wbBuf

	runSafePointFn uint32 // 如果为 1, 则在下一个 safe-point 运行 sched.safePointFn

	pad [sys.CacheLineSize]byte
}

type schedt struct {
	// accessed atomically. keep at top to ensure alignment on 32-bit systems.
	// 应该被原子访问。保持在第一个字段来确保 32 位系统上的对齐
	goidgen  uint64
	lastpoll uint64

	lock mutex

	// When increasing nmidle, nmidlelocked, nmsys, or nmfreed, be
	// sure to call checkdead().

	midle        muintptr // idle m's waiting for work
	nmidle       int32    // number of idle m's waiting for work
	nmidlelocked int32    // number of locked m's waiting for work
	mnext        int64    // 已经创建的 m 的个数，同时还表示下一个 m 的 id
	maxmcount    int32    // 允许（或死亡）的 m 的最大值
	nmsys        int32    // number of system m's not counted for deadlock
	nmfreed      int64    // cumulative number of freed m's

	ngsys uint32 // 系统 goroutine 的数量，动态更新

	pidle      puintptr // 空闲 p 链表
	npidle     uint32   // 空闲 p 数量
	nmspinning uint32   // 见 proc.go 中关于 "Worker thread parking/unparking" 的注释.

	// 全局 runnable 队列
	runqhead guintptr
	runqtail guintptr
	runqsize int32

	// 有效 dead G 的全局缓存.
	gflock       mutex
	gfreeStack   *g
	gfreeNoStack *g
	ngfree       int32

	// Central cache of sudog structs.
	sudoglock  mutex
	sudogcache *sudog

	// Central pool of available defer structs of different sizes.
	deferlock mutex
	deferpool [5]*_defer

	// freem is the list of m's waiting to be freed when their
	// m.exited is set. Linked through m.freelink.
	freem *m

	gcwaiting  uint32 // gc is waiting to run
	stopwait   int32
	stopnote   note
	sysmonwait uint32
	sysmonnote note

	// safepointFn should be called on each P at the next GC
	// safepoint if p.runSafePointFn is set.
	safePointFn   func(*p)
	safePointWait int32
	safePointNote note

	profilehz int32 // cpu profiling rate

	procresizetime int64 // 上一次修改 gomaxprocs 的时间 nanotime()
	totaltime      int64 // ∫gomaxprocs dt 在 procresizetime 的积分（总和）
}

// Values for the flags field of a sigTabT.
const (
	_SigNotify   = 1 << iota // let signal.Notify have signal, even if from kernel
	_SigKill                 // if signal.Notify doesn't take it, exit quietly
	_SigThrow                // if signal.Notify doesn't take it, exit loudly
	_SigPanic                // if the signal is from the kernel, panic
	_SigDefault              // if the signal isn't explicitly requested, don't monitor it
	_SigGoExit               // cause all runtime procs to exit (only used on Plan 9).
	_SigSetStack             // add SA_ONSTACK to libc handler
	_SigUnblock              // always unblock; see blockableSig
	_SigIgn                  // _SIG_DFL action is to ignore the signal
)

// Layout of in-memory per-function information prepared by linker
// See https://golang.org/s/go12symtab.
// Keep in sync with linker (../cmd/link/internal/ld/pcln.go:/pclntab)
// and with package debug/gosym and with symtab.go in package runtime.
type _func struct {
	entry   uintptr // start pc
	nameoff int32   // function name

	args   int32  // in/out args size
	funcID funcID // set for certain special runtime functions

	pcsp      int32
	pcfile    int32
	pcln      int32
	npcdata   int32
	nfuncdata int32
}

// layout of Itab known to compilers
// allocated in non-garbage-collected memory
// Needs to be in sync with
// ../cmd/compile/internal/gc/reflect.go:/^func.dumptypestructs.
type itab struct {
	inter *interfacetype
	_type *_type
	hash  uint32 // copy of _type.hash. Used for type switches.
	_     [4]byte
	fun   [1]uintptr // variable sized. fun[0]==0 means _type does not implement inter.
}

// Lock-free stack node.
// // Also known to export_test.go.
type lfnode struct {
	next    uint64
	pushcnt uintptr
}

type forcegcstate struct {
	lock mutex
	g    *g
	idle uint32
}

// startup_random_data holds random bytes initialized at startup. These come from
// the ELF AT_RANDOM auxiliary vector (vdso_linux_amd64.go or os_linux_386.go).
var startupRandomData []byte

// extendRandom extends the random numbers in r[:n] to the whole slice r.
// Treats n<0 as n==0.
func extendRandom(r []byte, n int) {
	if n < 0 {
		n = 0
	}
	for n < len(r) {
		// Extend random bits using hash function & time seed
		w := n
		if w > 16 {
			w = 16
		}
		h := memhash(unsafe.Pointer(&r[n-w]), uintptr(nanotime()), uintptr(w))
		for i := 0; i < sys.PtrSize && n < len(r); i++ {
			r[n] = byte(h)
			n++
			h >>= 8
		}
	}
}

// A _defer holds an entry on the list of deferred calls.
// If you add a field here, add code to clear it in freedefer.
type _defer struct {
	siz     int32
	started bool
	sp      uintptr // sp at time of defer
	pc      uintptr
	fn      *funcval
	_panic  *_panic // panic that is running defer
	link    *_defer
}

// A _panic holds information about an active panic.
//
// This is marked go:notinheap because _panic values must only ever
// live on the stack.
//
// The argp and link fields are stack pointers, but don't need special
// handling during stack growth: because they are pointer-typed and
// _panic values only live on the stack, regular stack pointer
// adjustment takes care of them.
//
//go:notinheap
type _panic struct {
	argp      unsafe.Pointer // pointer to arguments of deferred call run during panic; cannot move - known to liblink
	arg       interface{}    // argument to panic
	link      *_panic        // link to earlier panic
	recovered bool           // whether this panic is over
	aborted   bool           // the panic was aborted
}

// stack traces
type stkframe struct {
	fn       funcInfo   // function being run
	pc       uintptr    // program counter within fn
	continpc uintptr    // program counter where execution can continue, or 0 if not
	lr       uintptr    // program counter at caller aka link register
	sp       uintptr    // stack pointer at pc
	fp       uintptr    // stack pointer at caller aka frame pointer
	varp     uintptr    // top of local variables
	argp     uintptr    // pointer to function arguments
	arglen   uintptr    // number of bytes at argp
	argmap   *bitvector // force use of this argmap
}

// ancestorInfo records details of where a goroutine was started.
type ancestorInfo struct {
	pcs  []uintptr // pcs from the stack of this goroutine
	goid int64     // goroutine id of this goroutine; original goroutine possibly dead
	gopc uintptr   // pc of go statement that created this goroutine
}

const (
	_TraceRuntimeFrames = 1 << iota // include frames for internal runtime functions.
	_TraceTrap                      // the initial PC, SP are from a trap, not a return PC from a call
	_TraceJumpStack                 // if traceback is on a systemstack, resume trace at g that called into it
)

// The maximum number of frames we print for a traceback
const _TracebackMaxFrames = 100

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
type waitReason uint8

const (
	waitReasonZero                  waitReason = iota // ""
	waitReasonGCAssistMarking                         // "GC assist marking"
	waitReasonIOWait                                  // "IO wait"
	waitReasonChanReceiveNilChan                      // "chan receive (nil chan)"
	waitReasonChanSendNilChan                         // "chan send (nil chan)"
	waitReasonDumpingHeap                             // "dumping heap"
	waitReasonGarbageCollection                       // "garbage collection"
	waitReasonGarbageCollectionScan                   // "garbage collection scan"
	waitReasonPanicWait                               // "panicwait"
	waitReasonSelect                                  // "select"
	waitReasonSelectNoCases                           // "select (no cases)"
	waitReasonGCAssistWait                            // "GC assist wait"
	waitReasonGCSweepWait                             // "GC sweep wait"
	waitReasonChanReceive                             // "chan receive"
	waitReasonChanSend                                // "chan send"
	waitReasonFinalizerWait                           // "finalizer wait"
	waitReasonForceGGIdle                             // "force gc (idle)"
	waitReasonSemacquire                              // "semacquire"
	waitReasonSleep                                   // "sleep"
	waitReasonSyncCondWait                            // "sync.Cond.Wait"
	waitReasonTimerGoroutineIdle                      // "timer goroutine (idle)"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
)

var waitReasonStrings = [...]string{
	waitReasonZero:                  "",
	waitReasonGCAssistMarking:       "GC assist marking",
	waitReasonIOWait:                "IO wait",
	waitReasonChanReceiveNilChan:    "chan receive (nil chan)",
	waitReasonChanSendNilChan:       "chan send (nil chan)",
	waitReasonDumpingHeap:           "dumping heap",
	waitReasonGarbageCollection:     "garbage collection",
	waitReasonGarbageCollectionScan: "garbage collection scan",
	waitReasonPanicWait:             "panicwait",
	waitReasonSelect:                "select",
	waitReasonSelectNoCases:         "select (no cases)",
	waitReasonGCAssistWait:          "GC assist wait",
	waitReasonGCSweepWait:           "GC sweep wait",
	waitReasonChanReceive:           "chan receive",
	waitReasonChanSend:              "chan send",
	waitReasonFinalizerWait:         "finalizer wait",
	waitReasonForceGGIdle:           "force gc (idle)",
	waitReasonSemacquire:            "semacquire",
	waitReasonSleep:                 "sleep",
	waitReasonSyncCondWait:          "sync.Cond.Wait",
	waitReasonTimerGoroutineIdle:    "timer goroutine (idle)",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

var (
	allglen    uintptr
	allm       *m
	allp       []*p  // 所有 P 的存储位置，P 的个数是可以动态调整的
	allpLock   mutex // Protects P-less reads of allp and all writes
	gomaxprocs int32
	ncpu       int32
	forcegc    forcegcstate
	sched      schedt
	newprocs   int32

	// Information about what cpu features are available.
	// Set on startup in runtime.cpuinit.
	// Packages outside the runtime should not use these
	// as they are not an external api.
	// TODO: deprecate these; use internal/cpu directly.
	processorVersionInfo uint32
	isIntel              bool
	lfenceBeforeRdtsc    bool

	// Set in runtime.cpuinit.
	support_erms          bool
	support_popcnt        bool
	support_sse2          bool
	support_sse41         bool
	arm64_support_atomics bool

	goarm                uint8 // set by cmd/link on arm systems
	framepointer_enabled bool  // set by cmd/link
)

// Set by the linker so the runtime can determine the buildmode.
var (
	islibrary bool // -buildmode=c-shared
	isarchive bool // -buildmode=c-archive
)
