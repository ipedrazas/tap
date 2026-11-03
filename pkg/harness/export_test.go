package harness

import "context"

// RunTurnForTest runs one hook-triggered turn in a new session, as the
// trigger port does, for tests outside the package.
func RunTurnForTest(s *Server, who, message string, emit func(Event)) error {
	id := newID()
	sess := s.session(who, id)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return s.runTurn(context.Background(), who, id, "hook", sess, message, emit)
}
