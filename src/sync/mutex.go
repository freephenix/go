// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
type Mutex struct {
	state int32
	sema  uint32
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked = 1 << iota // mutex is locked
	mutexWoken
	mutexStarving
	mutexWaiterShift = iota

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	// 如果mutext的state没有被锁，也没有等待/唤醒的goroutine, 锁处于正常状态，那么获得锁，返回.
	// 比如锁第一次被goroutine请求时，就是这种状态。或者锁处于空闲的时候，也是这种状态。
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	m.lockSlow()
}

// TryLock tries to lock m and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
func (m *Mutex) TryLock() bool {
	old := m.state
	if old&(mutexLocked|mutexStarving) != 0 {
		return false
	}

	// There may be a goroutine waiting for the mutex, but we are
	// running now and can try to grab the mutex before that
	// goroutine wakes up.
	if !atomic.CompareAndSwapInt32(&m.state, old, old|mutexLocked) {
		return false
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
	return true
}

func (m *Mutex) lockSlow() {
	// 标记本goroutine的等待时间
	var waitStartTime int64
	// 本goroutine是否已经处于饥饿状态
	starving := false
	// 本goroutine是否已唤醒
	awoke := false
	// 自旋次数
	iter := 0
	// 复制锁的当前状态
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// mutexLocked|mutexStarving 将锁定和饥饿两个状态位置1
		// old与时，测试old中锁定和饥饿的两个状态位，==mutexLocked 表明old中锁定状态位为1，饥饿状态位为0
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			// 当前goroutine没有唤醒，锁也没有被唤醒，
			// mutexWaiterShift 为暂不使用的位置记录值，暂不使用的位置暂时用来存储等待goroutine数量
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			// 自旋
			runtime_doSpin()
			iter++
			old = m.state
			continue
		}
		// 到了这一步， state的状态可能是：
		// 1. 锁还没有被释放，锁处于正常状态
		// 2. 锁还没有被释放， 锁处于饥饿状态
		// 3. 锁已经被释放， 锁处于正常状态
		// 4. 锁已经被释放， 锁处于饥饿状态
		//
		// 并且本gorutine的 awoke可能是true, 也可能是false (其它goutine已经设置了state的woken标识)

		// new 复制 state的当前状态， 用来设置新的状态
		// old 是锁当前的状态
		new := old

		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		// 如果old state状态不是饥饿状态, new state 设置锁， 尝试通过CAS获取锁,
		// 如果old state状态是饥饿状态, 则不设置new state的锁，因为饥饿状态下锁直接转给等待队列的第一个.
		// 通过不释放mutexLocked位的方式，实现饥饿模式下，防止其他goroutine获取锁，
		// 直接转给等待队列第一个是有信号量实现的
		// 暂时先不着急直接改变锁状态，全部位处理后一次性操作
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}

		// 锁定状态或者饥饿状态时，将等待队列的等待者的数量加1（此goroutine本身）
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// starving是当前goroutine是否处于饥饿状态的标志，本goroutine已经饥饿，而且此时锁已经被锁定，
		// 也就意味着本goroutine无法获取锁，所以将new state的状态标记为饥饿状态, 将锁转变为饥饿状态.
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}

		// 如果本goroutine已经设置为唤醒状态, 需要清除new state的唤醒标记, 因为本goroutine要么获得了锁，要么进入休眠，
		// 总之state的新状态不再是woken状态.
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			// 如果goroutine唤醒，但是锁未唤醒，那属于一个不应该存在的状态
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			// 如果goroutine唤醒，锁也唤醒，那在执行完此次代码后，要么拿到锁，要么休眠，所以新的状态中清理掉锁唤醒标记
			new &^= mutexWoken
		}

		// 通过CAS设置new state值.
		// 注意new的锁标记不一定是1, 也可能只是标记一下锁的state是饥饿状态.
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 如果old state的状态是未被锁状态，并且锁不处于饥饿状态,
			// 那么当前goroutine已经获取了锁的拥有权，返回
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			// 设置/计算本goroutine的等待时间
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			// 既然未能获取到锁， 那么就使用sleep原语阻塞本goroutine
			// 如果是新来的goroutine, queueLifo=false, 加入到等待队列的尾部，耐心等待
			// 如果是唤醒的goroutine, queueLifo=true, 加入到等待队列的头部
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)
			// sleep之后，此goroutine被唤醒
			// 计算当前goroutine是否已经处于饥饿状态.
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			// 得到当前的锁状态
			old = m.state
			// 如果当前的state已经是饥饿状态
			// 那么锁应该处于Unlock状态，且锁被直接交给了本goroutine
			// 所以，此时锁应该还没有开始标记锁定、唤醒、且存在等待着
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				// 如果当前的state已被锁，或者已标记为唤醒， 或者等待的队列中不为空,
				// 那么state是一个非法状态
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// 当前goroutine用来设置锁，并将等待的goroutine数减1.
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				// 如果本goroutine是最后一个等待者，或者它并不处于饥饿状态，
				// 那么我们需要把锁的state状态设置为正常模式.
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					// 退出饥饿模式
					delta -= mutexStarving
				}
				// 设置新state, 因为已经获得了锁，退出、返回
				atomic.AddInt32(&m.state, delta)
				break
			}
			// 如果当前的锁是正常模式，本goroutine被唤醒，自旋次数清零，从for循环开始处重新开始
			awoke = true
			iter = 0
		} else {
			// 如果CAS不成功，重新获取锁的state, 从for循环开始处重新开始
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	// 如果state不是处于锁的状态, 那么就是Unlock根本没有加锁的mutex, panic
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	// 锁处于是正常状态
	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			// 如果没有等待的goroutine, 或者锁不处于空闲的状态，直接返回
			// 说明此时，此goroutine没有必要 或者 没有资格解锁，
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			// 将等待的goroutine数减一，并设置woken标识
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			// 设置新的state, 这里通过信号量会唤醒一个阻塞的goroutine去获取锁.
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		// 饥饿模式下， 直接将锁的拥有权传给等待队列中的第一个.
		// 注意此时state的mutexLocked还没有加锁，唤醒的goroutine会设置它。
		// 在此期间，如果有新的goroutine来请求锁， 因为mutex处于饥饿状态， mutex还是被认为处于锁状态，
		// 新来的goroutine不会把锁抢过去.
		runtime_Semrelease(&m.sema, true, 1)
	}
}
