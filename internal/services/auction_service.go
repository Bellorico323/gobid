package services

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type MessageKind int

const (
	// Requests
	PlaceBid MessageKind = iota

	// OK/Success
	SuccessfullyPlaceBid

	// Errors
	FailedToPlaceBid
	InvalidJSON

	// Info
	NewBidPlaced
	AuctionFinished
)

type Message struct {
	UserId  uuid.UUID   `json:"userId,omitempty"`
	Amount  float64     `json:"amount,omitempty"`
	Message string      `json:"message,omitempty"`
	Kind    MessageKind `json:"kind"`
}

type AuctionLobby struct {
	Rooms map[uuid.UUID]*AuctionRoom
	sync.Mutex
}

type AuctionRoom struct {
	Id         uuid.UUID
	Context    context.Context
	Broadcast  chan Message
	Register   chan *Client
	Unregister chan *Client
	Clients    map[uuid.UUID]*Client

	BidsService *BidsService
}

func (r *AuctionRoom) registerClient(c *Client) {
	slog.Info("New user connected", "Client", c)
	r.Clients[c.UserId] = c
}

func (r *AuctionRoom) unregisterClient(c *Client) {
	slog.Info("User disconnected", "Client", c)
	delete(r.Clients, c.UserId)
}

func (r *AuctionRoom) broadcastMessage(m Message) {
	slog.Info("New message received", "RoomId", r.Id, "message", m.Message, "UserId", m.UserId)

	switch m.Kind {
	case PlaceBid:
		bid, err := r.BidsService.PlaceBid(r.Context, r.Id, m.UserId, m.Amount)
		if err != nil {
			if errors.Is(err, ErrBidIsTooLow) {
				if client, ok := r.Clients[m.UserId]; ok {
					client.Send <- Message{
						Kind:    FailedToPlaceBid,
						Message: ErrBidIsTooLow.Error(),
						UserId:  m.UserId,
					}
				}
			}
			return
		}

		if client, ok := r.Clients[m.UserId]; ok {
			client.Send <- Message{
				Kind:    SuccessfullyPlaceBid,
				Message: "Your bid was successfully placed.",
				UserId:  m.UserId,
			}
		}

		for id, client := range r.Clients {
			newBidMessage := Message{
				Kind:    NewBidPlaced,
				Message: "A new bid was placed",
				Amount:  bid.BidAmount,
				UserId:  m.UserId,
			}

			if id == m.UserId {
				continue
			}
			client.Send <- newBidMessage

		}

	case InvalidJSON:
		client, ok := r.Clients[m.UserId]
		if !ok {
			slog.Info("Client not found in hashmap", "user_id", m.UserId)
			return
		}
		client.Send <- m
	}

}

func (r *AuctionRoom) Run() {
	slog.Info("Auction has begun", "auctionId", r.Id)

	defer func() {
		close(r.Broadcast)
		close(r.Register)
		close(r.Unregister)
	}()

	for {
		select {
		case client := <-r.Register:
			r.registerClient(client)
		case client := <-r.Unregister:
			r.unregisterClient(client)
		case message := <-r.Broadcast:
			r.broadcastMessage(message)
		case <-r.Context.Done():
			slog.Info("Auction has ended", "auctionID", r.Id)
			for _, client := range r.Clients {
				client.Send <- Message{Kind: AuctionFinished, Message: "auction has been finished"}
			}
			return
		}
	}
}

func NewAuctionRoom(ctx context.Context, id uuid.UUID, bidsService BidsService) *AuctionRoom {
	return &AuctionRoom{
		Id:          id,
		Broadcast:   make(chan Message),
		Register:    make(chan *Client),
		Unregister:  make(chan *Client),
		Clients:     make(map[uuid.UUID]*Client),
		Context:     ctx,
		BidsService: &bidsService,
	}
}

type Client struct {
	Room   *AuctionRoom
	Conn   *websocket.Conn
	Send   chan Message
	UserId uuid.UUID
}

func NewClient(room *AuctionRoom, conn *websocket.Conn, userId uuid.UUID) *Client {
	return &Client{
		Room:   room,
		Conn:   conn,
		Send:   make(chan Message, 512),
		UserId: userId,
	}
}

const (
	maxMessageSize = 512
	readDeadLine   = 60 * time.Second
	pingPeriod     = (readDeadLine * 9) / 10
	writeWait      = 10 * time.Second
)

func (c *Client) ReadEventLoop() {
	defer func() {
		c.Room.Unregister <- c
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(maxMessageSize)
	c.Conn.SetReadDeadline(time.Now().Add(readDeadLine))
	c.Conn.SetPongHandler(func(appData string) error {
		c.Conn.SetReadDeadline(time.Now().Add(readDeadLine))
		return nil
	})

	for {
		var m Message
		m.UserId = c.UserId
		err := c.Conn.ReadJSON(&m)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Error("Unexpected close error", "error", err)
				return
			}

			c.Room.Broadcast <- Message{
				Kind:    InvalidJSON,
				Message: "This message should be a valid json",
				UserId:  m.UserId,
			}
		}

		c.Room.Broadcast <- m
	}
}

func (c *Client) WriteEventLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			if !ok {
				c.Conn.WriteJSON(Message{
					Kind:    websocket.CloseMessage,
					Message: "Closing weboscket connection",
				})
				return
			}

			if message.Kind == AuctionFinished {
				close(c.Send)
				return
			}

			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.Conn.WriteJSON(message)
			if err != nil {
				c.Room.Unregister <- c
				return
			}

		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				slog.Error("Unexpected write error", "error", err)
				return
			}
		}
	}
}
