// Package mediaapi — gRPC-контракт между signal и media (см. media.proto).
package mediaapi

import (
	"context"
	"encoding/json"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
)

const ServiceName = "kontakt.media.v1.Media"

type CreateEndpointRequest struct {
	CallID      string `json:"call_id"`
	Transport   string `json:"transport"`
	PayloadType int32  `json:"payload_type"`
	LineMode    string `json:"line_mode"`
	RemoteIP    string `json:"remote_ip,omitempty"`
	RemotePort  int32  `json:"remote_port,omitempty"`
}

type Endpoint struct {
	ID        string `json:"id"`
	Transport string `json:"transport"`
	LocalIP   string `json:"local_ip,omitempty"`
	LocalPort int32  `json:"local_port,omitempty"`
	WSPath    string `json:"ws_path,omitempty"`
}

type BridgeRequest struct {
	A string `json:"a"`
	B string `json:"b"`
}

type DeleteRequest struct {
	ID string `json:"id"`
}

type SetToneRequest struct {
	ID      string  `json:"id"`
	Tone    string  `json:"tone"`
	Seconds float64 `json:"seconds,omitempty"`
	Then    string  `json:"then,omitempty"`
}

type SetLineRequest struct {
	ID       string `json:"id"`
	LineMode string `json:"line_mode"`
}

type Empty struct{}

type StatsReply struct {
	Endpoints  int32  `json:"endpoints"`
	Bridges    int32  `json:"bridges"`
	PacketsIn  uint64 `json:"packets_in"`
	PacketsOut uint64 `json:"packets_out"`
}

// MediaServer — то, что реализует медиа-сервис.
type MediaServer interface {
	CreateEndpoint(context.Context, *CreateEndpointRequest) (*Endpoint, error)
	Bridge(context.Context, *BridgeRequest) (*Empty, error)
	Delete(context.Context, *DeleteRequest) (*Empty, error)
	SetTone(context.Context, *SetToneRequest) (*Empty, error)
	SetLine(context.Context, *SetLineRequest) (*Empty, error)
	Stats(context.Context, *Empty) (*StatsReply, error)
}

// ---------- кодек ----------

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)   { return json.Marshal(v) }
func (jsonCodec) Unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func (jsonCodec) Name() string                    { return "json" }
func init()                                       { encoding.RegisterCodec(jsonCodec{}) }

// ---------- сервер ----------

func unary[Req, Resp any](name string, call func(MediaServer, context.Context, *Req) (*Resp, error)) grpc.MethodDesc {
	return grpc.MethodDesc{
		MethodName: name,
		Handler: func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
			in := new(Req)
			if err := dec(in); err != nil {
				return nil, err
			}
			if ic == nil {
				return call(srv.(MediaServer), ctx, in)
			}
			info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + name}
			return ic(ctx, in, info, func(ctx context.Context, req any) (any, error) {
				return call(srv.(MediaServer), ctx, req.(*Req))
			})
		},
	}
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: ServiceName,
	HandlerType: (*MediaServer)(nil),
	Methods: []grpc.MethodDesc{
		unary("CreateEndpoint", MediaServer.CreateEndpoint),
		unary("Bridge", MediaServer.Bridge),
		unary("Delete", MediaServer.Delete),
		unary("SetTone", MediaServer.SetTone),
		unary("SetLine", MediaServer.SetLine),
		unary("Stats", MediaServer.Stats),
	},
	Metadata: "media.proto",
}

func RegisterMediaServer(s *grpc.Server, impl MediaServer) { s.RegisterService(&serviceDesc, impl) }

// ---------- клиент ----------

type Client struct{ conn *grpc.ClientConn }

func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.CallContentSubtype("json")))
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func invoke[Resp any](c *Client, method string, in any) (*Resp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out := new(Resp)
	err := c.conn.Invoke(ctx, "/"+ServiceName+"/"+method, in, out)
	return out, err
}

func (c *Client) CreateEndpoint(r *CreateEndpointRequest) (*Endpoint, error) {
	return invoke[Endpoint](c, "CreateEndpoint", r)
}
func (c *Client) Bridge(a, b string) error {
	_, err := invoke[Empty](c, "Bridge", &BridgeRequest{A: a, B: b})
	return err
}
func (c *Client) Delete(id string) error {
	_, err := invoke[Empty](c, "Delete", &DeleteRequest{ID: id})
	return err
}
func (c *Client) SetTone(id, tone string, seconds float64, then string) error {
	_, err := invoke[Empty](c, "SetTone", &SetToneRequest{ID: id, Tone: tone, Seconds: seconds, Then: then})
	return err
}
func (c *Client) SetLine(id, mode string) error {
	_, err := invoke[Empty](c, "SetLine", &SetLineRequest{ID: id, LineMode: mode})
	return err
}
func (c *Client) Stats() (*StatsReply, error) { return invoke[StatsReply](c, "Stats", &Empty{}) }
