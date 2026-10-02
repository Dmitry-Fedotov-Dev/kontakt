module kontakt

go 1.24.7

require (
	github.com/gorilla/websocket v1.5.3
	google.golang.org/grpc v1.65.0
)

require (
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/sys v0.20.0 // indirect
	golang.org/x/text v0.15.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240528184218-531527333157 // indirect
	google.golang.org/protobuf v1.34.1 // indirect
)

// Подмены ниже нужны были только в среде сборки, где google.golang.org и golang.org недоступны.
// На обычной машине их можно удалить и выполнить `go mod tidy`.
replace (
	golang.org/x/net => github.com/golang/net v0.25.0
	golang.org/x/sys => github.com/golang/sys v0.20.0
	golang.org/x/text => github.com/golang/text v0.15.0
	google.golang.org/genproto/googleapis/rpc => github.com/googleapis/go-genproto/googleapis/rpc v0.0.0-20240528184218-531527333157
	google.golang.org/grpc => github.com/grpc/grpc-go v1.65.0
	google.golang.org/protobuf => github.com/protocolbuffers/protobuf-go v1.34.1
)
