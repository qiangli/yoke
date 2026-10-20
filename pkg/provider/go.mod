module github.com/qiangli/yoke/pkg/provider

// Nested, stdlib-only module (S472): cloudbox/hub stays on Go 1.26 and links
// this contract, the dispatcher and the DigitalOcean adapter directly.
go 1.26

toolchain go1.26.9
