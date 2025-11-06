# Docs

Just keeping things tidy here because I'll lose stuff and will have to
rediscover things again, will be replaced with docs later and any development
related things will move into a `contribute.md`.

## Anchor

Raydium CPMM/CP-Swap is Anchor _ready_? Is this a thing? I don't know, it's
anchor native. In any case, the IDL
[raydium_cp_swap.json](./raydium_cp_swap.json) is generated with:

```shell
anchor idl fetch -o raydium_cp_swap.json CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C
```

<details>
<summary>old glibc</summary>
Anchor straight from their _fast track_ instructions didn't work out of the box for me, I'm on GLIBC 2.35. So I had to
build from source.
</details>

Then I need to generate a Go package following the IDL, done through
[anchor-go](https://github.com/gagliardetto/anchor-go)

```shell
# first get it
go get -tool github.com/gagliardetto/anchor-go
# then use it
go tool anchor-go -idl ./raydium_cp_swap.json -output ./raydium_cp_swap -program-id CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C
```

Before moving on, we remove the `go.mod` and `go.sum` files from the generated
package.
