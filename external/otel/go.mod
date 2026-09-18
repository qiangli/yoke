module github.com/qiangli/yoke/external/otel

go 1.26.5

require (
	github.com/qiangli/yoke v0.0.0
	github.com/spf13/cobra v1.10.2
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
)

replace github.com/jaegertracing/jaeger => github.com/qiangli/jaeger v0.0.0-20260426223533-5aaa7eb1f040

replace github.com/perses/perses => github.com/qiangli/perses v0.0.0-20260426190059-de437951b5e6

replace github.com/qiangli/yoke => ../..
