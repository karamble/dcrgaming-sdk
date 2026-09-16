package transport

// ConnectionState describes the event subscription, not merely a successful Hello.
type ConnectionState string

const (
	Connecting   ConnectionState = "connecting"
	Subscribed   ConnectionState = "subscribed"
	Reconnecting ConnectionState = "reconnecting"
	Stopped      ConnectionState = "stopped"
)

// ConnectionStatus returns the current state and a coalescing notification
// channel. Consumers reread state on notification; a slow UI cannot block traffic.
func (c *Bridge) ConnectionStatus() (ConnectionState, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statusChanged == nil {
		c.statusChanged = make(chan struct{}, 1)
	}
	state := c.status
	if state == "" {
		state = Stopped
	}
	return state, c.statusChanged
}
func (c *Bridge) setStatus(state ConnectionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = state
	if c.statusChanged == nil {
		c.statusChanged = make(chan struct{}, 1)
	}
	select {
	case c.statusChanged <- struct{}{}:
	default:
	}
}
