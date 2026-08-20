module github.com/recurser/bossd-plugin-dependabot

go 1.25.7

require (
	github.com/hashicorp/go-plugin v1.8.0
	github.com/recurser/bossalib v0.0.0
	github.com/rs/zerolog v1.35.1
	google.golang.org/grpc v1.83.0
)

require (
	github.com/fatih/color v1.13.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/hashicorp/go-hclog v1.6.3 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.23 // indirect
	github.com/oklog/run v1.1.0 // indirect
	github.com/qmuntal/stateless v1.8.0 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260720211330-0afa2a65878a // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/recurser/bossalib => ../../lib/bossalib
