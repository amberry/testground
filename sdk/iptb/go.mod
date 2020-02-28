module github.com/ipfs/testground/sdk/iptb

go 1.13

require (
	github.com/ipfs/go-ipfs-api v0.0.3
	github.com/ipfs/go-ipfs-config v0.2.0
	github.com/ipfs/iptb v1.4.0
	github.com/ipfs/iptb-plugins v0.2.1
	github.com/ipfs/testground/sdk/runtime v0.1.0
	github.com/multiformats/go-multiaddr v0.2.0
	github.com/niemeyer/pretty v0.0.0-20200227124842-a10e7caefd8e // indirect
	gopkg.in/check.v1 v1.0.0-20200227125254-8fa46927fb4f // indirect
)

replace github.com/ipfs/testground/sdk/runtime => ../runtime
