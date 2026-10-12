module github.com/qiangli/yoke/external/otel

go 1.27

toolchain go1.27.1

require (
	github.com/qiangli/yoke v0.0.0-20261012201704-ceaeba133437
	github.com/spf13/cobra v1.10.2
	go.opentelemetry.io/proto/otlp v1.10.0
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/ulikunitz/xz v0.5.15 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	mvdan.cc/sh/v3 v3.13.1 // indirect
)

replace github.com/jaegertracing/jaeger => github.com/qiangli/jaeger v0.0.0-20260426223533-5aaa7eb1f040

replace github.com/perses/perses => github.com/qiangli/perses v0.0.0-20260426190059-de437951b5e6

replace mvdan.cc/sh/v3 => github.com/qiangli/sh/v3 v3.0.0-20261009065157-d3d7766e8a47
