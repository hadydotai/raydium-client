# Contribute

One rule, two facts.

1. Don't be an ass.
1. Code is provided as-is, if you don't like something change it and contribute
   back
1. Taking time to contribute, while appreciated, does not equal an obligation on
   my end to accept the contribution.

## Stack

This is a simple Go application for all intents and purposes. It depends on the
following core pieces

- [`solana-go`](https://github.com/gagliardetto/solana-go)
- [`anchor-go`](https://github.com/gagliardetto/anchor-go)
- [`termbox-go`](https://github.com/nsf/termbox-go)

> [!IMPORTANT]
> The following steps have already been done, this is just an account of what
> these steps are, why, and how, in case it needs to be redone for any reason.
> (eg. Updating Raydium's IDL)

### Anchor

Raydium CPMM/CP-Swap is based on Anchor, to interface with it we need Raydium's
[IDL](https://solana.com/de/developers/guides/advanced/idls) and we need to
generate some binding in Go for that IDL (so we don't have to do any bit mapping
ourselves, or need to figure out how to correctly construct Raydium's
instructions)

I've included [raydium_cp_swap.json](./raydium_cp_swap.json), which is the IDL
for Raydium CP-Swap program (CPMM pools), that was generated with the `anchor`
CLI.

```shell
anchor idl fetch -o raydium_cp_swap.json CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C
```

<details>
<summary>old glibc</summary>

If you're having trouble running anchor like I did, because of an old GLIBC,
just build from source. You'll obviously need a ready to use Rust toolchain
installed locally, but building from source will amount to some variation of

```shell
cargo build --release
ln -s <build/target/path> /usr/local/bin/anchor
```

That's about as much as you'll need just to be able to run `anchor idl fetch`. I
haven't looked into it extensively, but anchor's install script might be doing
more than this. Proceed at your own discretion.

</details>

<br />

Then you'll need to generate a Go package following the IDL we just fetched,
this is done through `anchor-go`.

```shell
# first get it
go get -tool github.com/gagliardetto/anchor-go
# then use it
go tool anchor-go -idl ./raydium_cp_swap.json -output ./raydium_cp_swap -program-id CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C
```

Before moving on, we remove the `go.mod` and `go.sum` files from the generated
package.
