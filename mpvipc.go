// Package mpvipc provides an interface for communicating with the mpv media
// player via it's JSON IPC interface
package mpvipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"sync"
)

// Connection represents a connection to a mpv IPC socket
type Connection struct {
	client     net.Conn
	socketName string

	lastRequest     uint
	waitingRequests map[uint]chan *commandResult

	lastListener   uint
	eventListeners map[uint]chan<- *Event

	lastCloseWaiter uint
	closeWaiters    map[uint]chan struct{}

	events chan *Event
	stop   chan struct{}

	lock *sync.Mutex
}

// Event represents an event received from mpv. For a list of all possible
// events, see https://mpv.io/manual/master/#list-of-events
type Event struct {
	// Name is the only obligatory field: the name of the event
	Name string `json:"event"`

	// Reason is the reason for the event: currently used for the "end-file"
	// event. When Name is "end-file", possible values of Reason are:
	// "eof", "stop", "quit", "error", "redirect", "unknown"
	Reason string `json:"reason"`

	// Prefix is the log-message prefix (only if Name is "log-message")
	Prefix string `json:"prefix"`

	// Level is the loglevel for a log-message (only if Name is "log-message")
	Level string `json:"level"`

	// Text is the text of a log-message (only if Name is "log-message")
	Text string `json:"text"`

	// ID is the user-set property ID (on events triggered by observed properties)
	ID uint `json:"id"`

	// Data is the property value (on events triggered by observed properties)
	Data interface{} `json:"data"`

	// ExtraData contains extra attributes of the event payload (on spontaneous events)
	ExtraData map[string]interface{} `json:"-"`
}

var eventFieldNames = []string{}

func init() {
	// collect all named fields into a global
	val := reflect.ValueOf(Event{})
	for i := 0; i < val.Type().NumField(); i++ {
		field := val.Type().Field(i).Tag.Get("json")
		if field != "-" {
			eventFieldNames = append(eventFieldNames, field)
		}
	}
}

func (e *Event) UnmarshalJSON(data []byte) error {
	type Proxy Event

	// unmarshal named fields
	if err := json.Unmarshal(data, (*Proxy)(e)); err != nil {
		return err
	}

	// unmarshal unnamed fields into our container
	if err := json.Unmarshal(data, &e.ExtraData); err != nil {
		return err
	}

	// drop all named fields from ExtraData
	for _, field := range eventFieldNames {
		delete(e.ExtraData, field)
	}

	return nil
}

// NewConnection returns a Connection associated with the given unix socket
func NewConnection(socketName string) *Connection {
	return &Connection{
		socketName:      socketName,
		lock:            &sync.Mutex{},
		waitingRequests: make(map[uint]chan *commandResult),
		eventListeners:  make(map[uint]chan<- *Event),
		closeWaiters:    make(map[uint]chan struct{}),
	}
}

// Open connects to the socket. Returns an error if already connected.
// It also starts listening to events, so ListenForEvents() can be called
// afterwards.
func (c *Connection) Open(events chan *Event, stop chan struct{}) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.client != nil {
		return fmt.Errorf("already open")
	}
	client, err := dial(c.socketName)
	if err != nil {
		return fmt.Errorf("can't connect to mpv's socket: %s", err)
	}
	c.client = client
	c.events = events
	c.stop = stop
	go c.listen(events, stop)
	return nil
}

// ListenForEvents blocks until something is received on the stop channel (or
// it's closed).
// In the mean time, events received on the socket will be sent on the events
// channel. They may not appear in the same order they happened in.
//
// The events channel is closed automatically just before this method returns.
func (c *Connection) ListenForEvents(events chan<- *Event, stop <-chan struct{}) {
	c.lock.Lock()
	c.lastListener++
	id := c.lastListener
	c.eventListeners[id] = events
	c.lock.Unlock()

	<-stop

	c.lock.Lock()
	delete(c.eventListeners, id)
	close(events)
	c.lock.Unlock()
}

// NewEventListener is a convenience wrapper around ListenForEvents(). It
// creates and returns the event channel and the stop channel. After calling
// NewEventListener, read events from the events channel and send an empty
// struct to the stop channel to close it.
func (c *Connection) NewEventListener() (chan *Event, chan struct{}) {
	events := make(chan *Event)
	stop := make(chan struct{})
	go c.ListenForEvents(events, stop)
	return events, stop
}

func (c *Connection) GetListeners() (chan *Event, chan struct{}) {
	return c.events, c.stop
}

// Call calls an arbitrary command and returns its result. For a list of
// possible functions, see https://mpv.io/manual/master/#commands and
// https://mpv.io/manual/master/#list-of-input-commands
func (c *Connection) Call(arguments ...interface{}) (interface{}, error) {
	c.lock.Lock()
	c.lastRequest++
	id := c.lastRequest
	resultChannel := make(chan *commandResult)
	c.waitingRequests[id] = resultChannel
	c.lock.Unlock()

	defer func() {
		c.lock.Lock()
		close(c.waitingRequests[id])
		delete(c.waitingRequests, id)
		c.lock.Unlock()
	}()

	err := c.sendCommand(id, arguments...)
	if err != nil {
		return nil, err
	}

	result := <-resultChannel
	if result.Status == "success" {
		return result.Data, nil
	}
	return nil, fmt.Errorf("mpv error: %s", result.Status)
}

// Set is a shortcut to Call("set_property", property, value)
func (c *Connection) Set(property string, value interface{}) error {
	_, err := c.Call("set_property", property, value)
	return err
}

// Get is a shortcut to Call("get_property", property)
func (c *Connection) Get(property string) (interface{}, error) {
	value, err := c.Call("get_property", property)
	return value, err
}

// Close closes the socket, disconnecting from mpv. It is safe to call Close()
// on an already closed connection.
func (c *Connection) Close() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.client != nil {
		err := c.client.Close()
		for waiterID := range c.closeWaiters {
			close(c.closeWaiters[waiterID])
		}
		c.client = nil
		return err
	}
	return nil
}

// IsClosed returns true if the connection is closed. There are several cases
// in which a connection is closed:
//
// 1. Close() has been called
//
// 2. The connection has been initialised but Open() hasn't been called yet
//
// 3. The connection terminated because of an error, mpv exiting or crashing
//
// It's ok to use IsClosed() to check if you need to reopen the connection
// before calling a command.
func (c *Connection) IsClosed() bool {
	return c.client == nil
}

// WaitUntilClosed blocks until the connection becomes closed. See IsClosed()
// for an explanation of the closed state.
func (c *Connection) WaitUntilClosed() {
	c.lock.Lock()
	if c.IsClosed() {
		c.lock.Unlock()
		return
	}

	closed := make(chan struct{})
	c.lastCloseWaiter++
	waiterID := c.lastCloseWaiter
	c.closeWaiters[waiterID] = closed

	c.lock.Unlock()

	<-closed

	c.lock.Lock()
	delete(c.closeWaiters, waiterID)
	c.lock.Unlock()
}

func (c *Connection) sendCommand(id uint, arguments ...interface{}) error {
	if c.client == nil {
		return fmt.Errorf("trying to send command on closed mpv client")
	}
	message := &commandRequest{
		Arguments: arguments,
		ID:        id,
	}
	data, err := json.Marshal(&message)
	if err != nil {
		return fmt.Errorf("can't encode command: %s", err)
	}
	_, err = c.client.Write(data)
	if err != nil {
		return fmt.Errorf("can't write command: %s", err)
	}
	_, err = c.client.Write([]byte("\n"))
	if err != nil {
		return fmt.Errorf("can't terminate command: %s", err)
	}
	return err
}

type commandRequest struct {
	Arguments []interface{} `json:"command"`
	ID        uint          `json:"request_id"`
}

type commandResult struct {
	Status string      `json:"error"`
	Data   interface{} `json:"data"`
	ID     uint        `json:"request_id"`
}

func (c *Connection) checkResult(data []byte) {
	result := &commandResult{}
	err := json.Unmarshal(data, &result)
	if err != nil {
		return // skip malformed data
	}
	if result.Status == "" {
		return // not a result
	}
	c.lock.Lock()
	request, ok := c.waitingRequests[result.ID]
	c.lock.Unlock()
	if ok {
		request <- result
	}
}

func (c *Connection) checkEvent(data []byte, events chan<- *Event, stop <-chan struct{}) {
	event := &Event{}
	err := json.Unmarshal(data, &event)
	if err != nil {
		return // skip malformed data
	}
	if event.Name == "" {
		return // not an event
	}
	c.lock.Lock()
	for listenerID := range c.eventListeners {
		select {
		case <-stop:
			close(events)
			return
		default:
			listener := c.eventListeners[listenerID]
			go func() {
				listener <- event
			}()
		}
	}
	c.lock.Unlock()
}

func (c *Connection) listen(events chan<- *Event, stop <-chan struct{}) {
	scanner := bufio.NewScanner(c.client)
	for scanner.Scan() {
		data := scanner.Bytes()
		c.checkEvent(data, events, stop)
		c.checkResult(data)
	}
	_ = c.Close()
}
