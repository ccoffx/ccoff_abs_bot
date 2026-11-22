package app

import "sync"

// Only the ready head of each user's queue may occupy a message worker.
// Callbacks share arrival order but wait outside the message worker pool.
type userUpdateTurn struct {
	userID    int64
	ready     chan struct{}
	next      *userUpdateTurn
	completed bool
	message   bool
	dispatch  func()
}
type userUpdateQueue struct{ head, tail *userUpdateTurn }

var updateOrder = struct {
	sync.Mutex
	users    map[int64]*userUpdateQueue
	messages int
}{users: make(map[int64]*userUpdateQueue)}

func reserveUserUpdateLocked(userID int64) *userUpdateTurn {
	turn := &userUpdateTurn{userID: userID, ready: make(chan struct{})}
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
func reserveUserUpdate(userID int64) *userUpdateTurn {
	updateOrder.Lock()
	defer updateOrder.Unlock()
	return reserveUserUpdateLocked(userID)
}
func reserveUserMessage(userID int64, capacity int) *userUpdateTurn {
	updateOrder.Lock()
	defer updateOrder.Unlock()
	if updateOrder.messages >= capacity {
		return nil
	}
	turn := reserveUserUpdateLocked(userID)
	turn.message = true
	updateOrder.messages++
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
func (turn *userUpdateTurn) whenReady(dispatch func()) {
	updateOrder.Lock()
	turn.dispatch = dispatch
	ready := turn.isReady() && !turn.completed
	updateOrder.Unlock()
	if ready {
		dispatch()
	}
}
func (turn *userUpdateTurn) finish() {
	if turn == nil {
		return
	}
	updateOrder.Lock()
	if turn.completed {
		updateOrder.Unlock()
		return
	}
	turn.completed = true
	if turn.message {
		updateOrder.messages--
	}
	queue := updateOrder.users[turn.userID]
	if queue == nil || queue.head != turn {
		updateOrder.Unlock()
		return
	}
	for queue.head != nil && queue.head.completed {
		queue.head = queue.head.next
	}
	var dispatch func()
	if queue.head == nil {
		delete(updateOrder.users, turn.userID)
	} else {
		close(queue.head.ready)
		dispatch = queue.head.dispatch
	}
	updateOrder.Unlock()
	if dispatch != nil {
		dispatch()
	}
}
