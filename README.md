# Raydium Client

Raydium client is a small CLI application to interact with Raydium, specifically
around swapping between tokens using Raydium's CPMM/CP-Swap pools.

It integrates directly with Raydium using only the blockchain, no API
frivolities. It can operate in two modes, interactive and non-interactive. By
default it's interactive (a mini TUI), but can be driven purely by flags on the
CLI.

## Prose & Contributions

- For the curious, I like to document my thought process in the code through
  `NOTE(@hadydotai):` comments, and there's an included [devlog.md](./devlog.md)
  file.
- For anyone interested in contributing, please read
  [contribute.md](./contribute.md) first.
