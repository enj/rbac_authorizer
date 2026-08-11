module github.com/enj/soapbox/tools

// The go directive matches the go directive of kubernetes/kubernetes v1.36.1
// so the engine language version never trails the source it transforms.
go 1.26.0

// The toolchain is pinned to an exact patch release because gofmt output and
// generated module metadata must be byte identical across machines.
toolchain go1.26.5

require gopkg.in/yaml.v3 v3.0.1
