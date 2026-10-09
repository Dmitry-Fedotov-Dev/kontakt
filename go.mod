module kontakt

go 1.24.7

require (
	github.com/gorilla/websocket v1.5.3
	github.com/pion/interceptor v0.1.49
	github.com/pion/rtp v1.10.5
	github.com/pion/webrtc/v4 v4.2.23
	google.golang.org/grpc v1.65.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/pion/datachannel v1.6.3 // indirect
	github.com/pion/dtls/v3 v3.1.10 // indirect
	github.com/pion/ice/v4 v4.4.7 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.2.2 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.19 // indirect
	github.com/pion/sctp v1.12.0 // indirect
	github.com/pion/sdp/v3 v3.0.20 // indirect
	github.com/pion/srtp/v3 v3.1.3 // indirect
	github.com/pion/stun/v4 v4.0.1 // indirect
	github.com/pion/transport/v5 v5.1.1 // indirect
	github.com/pion/turn/v5 v5.1.2 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/crypto v0.48.0 // indirect
	golang.org/x/net v0.50.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240528184218-531527333157 // indirect
	google.golang.org/protobuf v1.34.1 // indirect
)

// Подмены ниже нужны были только в среде сборки, где google.golang.org недоступен (golang.org/x —
// уже без подмен: pion/webrtc требует свежие x/net, x/sys).
// На обычной машине их можно удалить и выполнить `go mod tidy`.
replace (
	google.golang.org/genproto/googleapis/rpc => github.com/googleapis/go-genproto/googleapis/rpc v0.0.0-20240528184218-531527333157
	google.golang.org/grpc => github.com/grpc/grpc-go v1.65.0
	google.golang.org/protobuf => github.com/protocolbuffers/protobuf-go v1.34.1
)
