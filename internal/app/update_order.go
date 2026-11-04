package app

import "sync"

// Reserve turns when updates arrive, before workers or callback goroutines can
// race for the user lock. Different users remain independent.
type userUpdateTurn struct {
	userID    int64
	ready     chan struct{}
	next      *userUpdateTurn
	completed bool
}

type userUpdateQueue struct {
	head, tail *userUpdateTurn
}

var updateOrder = struct {
	sync.Mutex
	users map[int64]*userUpdateQueue
}{users: make(map[int64]*userUpdateQueue)}

func reserveUserUpdate(userID int64) *userUpdateTurn {
	turn := &userUpdateTurn{userID: userID, ready: make(chan struct{})}
	updateOrder.Lock()
	defer updateOrder.Unlock()
	queue := updateOrder.users[userID]
	if queue == nil {
		updateOrder.users[userID] = &userUpdateQueue{head: turn, tail: turn}
		close(turn.ready)
	} else {
		queue.tail.next = turn
		queue.tail = turn
	}
	return turn
}

func (turn *userUpdateTurn) isReady() bool {
	select {
	case <-turn.ready:
		return true
	default:
		return false
	}
}

func (turn *userUpdateTurn) wait() {
	if turn != nil {
		<-turn.ready
	}
}

func (turn *userUpdateTurn) finish() {
	if turn == nil {
		return
	}
	updateOrder.Lock()
	defer updateOrder.Unlock()
	if turn.completed {
		return
	}
	turn.completed = true
	queue := updateOrder.users[turn.userID]
	if queue.head != turn {
		// A full worker queue may reject a turn that has not started. Keep its
		// position until earlier turns finish, so cancellation cannot let a
		// later update overtake the operation that is still running.
		return
	}
	for queue.head != nil && queue.head.completed {
		queue.head = queue.head.next
	}
	if queue.head == nil {
		delete(updateOrder.users, turn.userID)
	} else {
		close(queue.head.ready)
	}
}
