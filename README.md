# Raydium Client

<a href="https://asciinema.org/a/754990" target="_blank"><img src="https://asciinema.org/a/754990.svg" /></a>

Raydium client is a small CLI application to interact with Raydium, specifically
around swapping between tokens using Raydium's CPMM/CP-Swap pools.

It integrates directly with Raydium using only the blockchain, no API
frivolities. It can operate in two modes, interactive and non-interactive. By
default it's interactive (a mini TUI), but can be driven purely by flags on the
CLI.

## Install

There's releases available on Github for all three platforms, windows, linux,
and mac. I'm a Linux/Windows user so naturally this got tested on both of these
platforms. I don't own a mac, so if you try it and run into any issues, feel
free to open an issue and I'll see if I can get it resolved.

For a quick run here's the latest release on linux:

```shell
wget https://github.com/hadydotai/raydium-client/releases/download/0.0.1-alpha/raydium-client-0.0.1-alpha-linux-amd64.tar.gz
tar xvf raydium-client-0.0.1-alpha-linux-amd64.tar.gz
./raydium-client-0.0.1-alpha --help
```

## How to use

> [!CAUTION]
> Be mindful of what pool you're interacting with. Don't spend money you can't
> afford to lose, and understand that this software is in an `alpha`.

### Quick Start

1. **Hot wallet**: export a Solana keypair file (e.g. from `solana-keygen`) and
   note the absolute path.
2. **Pick a pool**: browse
   [raydium.io/liquidity-pools](https://raydium.io/liquidity-pools/?tab=standard),
   hover any CPMM/CP-Swap pool, and copy the on-chain pool address.
3. **Run the CLI**: at minimum you must pass `-hotwallet`, `-pool`. You should
   also pass an `-intent`, although it's not required when starting in the
   _interactive_, it's nicer.

Example (interactive/default mode):

```shell
raydium-client-0.0.1-alpha \
  -hotwallet ~/.config/solana/devnet.json \
  -intent "pay 10 USDC" \
  -pool <poolID>
```

You will land in the TUI, see current pool balances, and be prompted for an intent
(see **Intent DSL** below).

### Modes

- **Interactive (default):** Starts the TUI where you can edit intents, rerun
  them, and view nicely formatted tables. Great for discovery because you can
  try intents repeatedly before committing.
- **Scriptable (`-no-tui`):** Skips the UI and runs a single intent provided via
  `-intent`. This is ideal for automation/cron jobs. When `-no-tui` is set, the
  `-intent` flag becomes mandatory and the program exits after executing (or
  reporting why it could not execute).

If any required flag is missing or malformed, the CLI prints a descriptive error
plus `-help` output and exits with code 2, so you always see what to fix.

### Flags at a Glance

| Flag         | Required?           | Description                                                                                     | Default         |
| ------------ | ------------------- | ----------------------------------------------------------------------------------------------- | --------------- |
| `-hotwallet` | yes                 | Path to the payer keypair file used for signing and paying fees.                                | _none_          |
| `-pool`      | yes                 | Raydium CP-Swap/CPMM pool address you want to trade against.                                    | _none_          |
| `-network`   | yes                 | Target cluster, `devnet` or `mainnet`. Also drives the default RPC choice.                      | `devnet`        |
| `-rpc`       | conditional         | Custom RPC endpoint. If omitted the client picks the canonical endpoint for the chosen network. | network default |
| `-intent`    | only when `-no-tui` | Intent DSL command that describes what you want to buy/sell.                                    | empty           |
| `-slippage`  | no                  | Slippage tolerance in percent (e.g. `0.5` = 0.5%). Applied when building swap instructions.     | `0.5`           |
| `-no-tui`    | no                  | Disable the interactive UI and run a single intent in batch mode.                               | `false`         |

### Intent DSL

The intent language is deliberately tiny so you can memorize it quickly:

```
<verb> <amount> <token-symbol>
```

- **Verbs:**
  - `pay`, `sell`, `swap` you specify how much of the given token you want to
    spend. The client figures out how much of the counter token you will
    receive.
  - `buy`, `get` you specify how much of the given token you want to receive.
    The client figures out the maximum amount of the counter token you must pay
- **Amount:** Accepts integers or decimals and is interpreted using the token’s
  decimals from the pool vaults.
- **Token symbol:** Case-insensitive ticker that must be mappable to one of the
  pool’s two mints. The first run against a new pool may ask you to confirm a
  symbol to mint mapping; once accepted, it’s cached for the session.

#### Examples

| Intent         | Effect                                                                                                                                          |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `pay 1 SOL`    | Spend exactly 1 SOL (token0 or token1 depending on the pool) and receive as much of the counter token as the curve returns after fees/slippage. |
| `sell 0.3 RAY` | Same as `pay`, just using the `sell` synonym.                                                                                                   |
| `buy 50 USDC`  | Acquire exactly 50 USDC from the pool, the CLI computes how much of the paired asset you must supply (and sets `MaxAmountIn` accordingly).      |
| `get 100 BONK` | Another `buy` synonym—handy when you care about the output amount.                                                                              |

Combine these with `-no-tui` for automation. Example batch run:

```shell
raydium-client-0.0.1-alpha \
  -network devnet \
  -rpc https://your.custom.rpc \
  -hotwallet ~/.config/solana/hot.json \
  -pool <poolID> \
  -slippage 0.35 \
  -intent "buy 25 USDC" \
  -no-tui
```

The command resolves the pool’s token pair, simulates the constant-product math,
prints a summary table, and then submits the swap transaction if all validations
pass.

## Limitiations

Please consider this pre-alpha software. Don't use it with money you're not
willing to lose, and if at all possible, switch to devnet.

Here's a non-exhaustive list of limitations that are currently in the code.

1. It ignores the status of a pool, and by ignore I mean I don't check for it.
2. It doesn't inform you of the price impact your trade would have
3. It doesn't attempt any recoveries, the code is littered with `log.Fatal`,
   this is intentional, I'm operating under a time budget and I intend to meet
   it. This means there are certain bridges that I'm not crossing. Something
   will fail, and when it does it won't rollback any effects if any have taken
   place. Luickly, we currently only do one thing which is a single hop
   swapping, but it's important to acknowledge this limitation regardless.
4. Only supports CPMM/CP-Swap pools
5. You'll need to give it a pool address, it'll complain if you give it a
   non-CPMM pool, so the responsibility falls on you to source the pool you want
   to trade from.
6. There's fancy tables, fancy interactive loop, no fancy terminal progress
   bars, yet.

## Prose & Contributions

- For the curious, I like to document my thought process in the code through
  `NOTE(@hadydotai):` comments, and there's an included [devlog.md](./devlog.md)
  file.
- For anyone interested in contributing, please read
  [contribute.md](./contribute.md) first.
