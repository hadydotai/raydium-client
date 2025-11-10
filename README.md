# Raydium Client

Raydium client is a small CLI application to interact with Raydium, specifically
around swapping between tokens using Raydium's CPMM/CP-Swap pools.

It integrates directly with Raydium using only the blockchain, no API
frivolities. It can operate in two modes, interactive and non-interactive. By
default it's interactive (a mini TUI), but can be driven purely by flags on the
CLI.

## Install

```shell
```

## How to use

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
6. We lazily resolve token symbols when we load the pool information, hence why
   I decided to go down that road. Initially I was asking for a token address,
   then realized pre-caching all token metadata at least for all CPMM pools is a
   tall order (not so much as complex as it is just outside the scope of this
   project and shouldn't really be part of it anyway), if we had a pre-cache
   then it would have been wonderful to ask you to supply a ticker symbol or a
   pair and then we filter the pools to get those. Since we don't have that, I
   decided to ask for a pool address and resolve backwards from there,
   identifying the pair symbols.
7. There's fancy tables, fancy interactive loop, no fancy terminal progress bars
   :) Sorry, time is a factor here, but I'll get to it soon enough.

## Prose & Contributions

- For the curious, I like to document my thought process in the code through
  `NOTE(@hadydotai):` comments, and there's an included [devlog.md](./devlog.md)
  file.
- For anyone interested in contributing, please read
  [contribute.md](./contribute.md) first.
