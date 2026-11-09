// The rewards platform's contracts, as a team publishing them would release them: plugins pin example.com/rewards
// at a version (internal/testproxy serves it offline to the teams' CI), and the platform builds against this
// checkout of it.
module example.com/rewards

go 1.27

// Only because the contracts live in plg's own repository: a contract module requires a released plg.
replace github.com/heynemann/go-plg => ../..

require github.com/heynemann/go-plg v0.0.0-00010101000000-000000000000

require (
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic v1.15.5 // indirect
	github.com/bytedance/sonic/loader v0.5.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/puzpuzpuz/xsync/v4 v4.5.0 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/arch v0.0.0-20210923205945-b76863e36670 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)
