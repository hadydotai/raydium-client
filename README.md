# Raydium Client

Hopefully will be a DeFi client at some point, for now I'm starting with Raydium
and figuring out my way around Solana. Curious things going on there.

Going to keep notes for myself, it's a lot of information to pack in under a
week. So, broad lines:

1. Accounts, _everything_ is an account in Solana. Some are executable, some
   aren't.
2. Programs, are statless accounts marked as executable.
3. Instructions, are the meat and potatoes of this whole thing, specifically
   around the order of account(s) metadata.

Interacting with an existing program on the chain means (1) we need to figure
out what instructions to call and (2) for each instruction, what inputs are
needed which is relatively straight forward if not for the annoyance of layering
the account metadata with the correct permissions in the correct order, some
going as far as being more than 12 accounts per call. Fine, if we have access to
the IDL, and or the program code which luckily we do for Raydium, then it's not
too bad.

We go from chewing glass to, well, I guess chewing chewed glass or maybe a
tasteless gum. Regardless, it's doable.

The other part is obviously some theater around each instruction, for example
some instructions take SPL tokens, so if we want to spend `SOL`, we have to
actually spend `wSOL` which is the SPL wrapped version of SOL this means we need
to call `SyncNative` to get the chain to sync things back and forth, and yeah.
Fuzzy around the edges at the moment but I'm sure once I start writing the code,
it'll clear up.

There's also the point of ATA (Associated Token Accounts), I think best
explained [here](https://solana.com/docs/tokens#associated-token-account). If I
simplify that without botching it, I'd say it just captures the relationship
between an owner of some tokens and the tokens themselves. How does that differ
from Token Accounts? Well, good question. I think reading through the docs, all
I can think of is the determinsim inherit to the fact ATA are derived from PDAs.
Program dervied addresses are an addressing scheme as far as I can tell, and for
every owner + mint address there's one ATA and only one. Because it's derived
from the PDA combining the owner address and the mint address as inputs.

How is that useful, is unclear to me at the moment. I can definitely see a few
reasons why deterministic and unique addressing is better. Out in the wild, I
see the ATAs being used a lot for staging operations, like wrapping up SOL for
spending on a program that has an instruction that needs an SPL token, like
this:

- Create ATA for wSOL if it doesn't already exist
- Send SOL (lamports) into the ATA, thereby _wrapping_ it essentially.
- Call SyncNative on the ATA
- Most importantly, have to close the ATA to unwrapp the SOL, and of course
  reclaim whatever _rent_ the account needed.

I reckon the rent is essentially similar to Polkadot's Staking Proxy accounts,
which requires some form of initial deposit or, not an existential fee (that is
true for all Polkadot accounts) but this one in particular, the Staking Proxy,
actually costs something to create one, and that fee is reclaimed when the proxy
is removed. That's how I'm imaging _rent_ in Solana.

I mean they both serve the same purpose really, paying for storage and upkeep.
Anyway, besides the point, ATAs, interesting stuff and seems necessary.

## Enter Raydium

I've been looking into Raydium for two days now, along with my deep dives into
Solana. Fucking mess...

Just kidding, kind of though. I want to interface with them without using APIs
or any such frivolity. We're raw dogging this!

What can we do on Raydium? A lot, but I'm picking Swapping for now.

Their swap UI will almost certainly trigger a swap on CLMM. CLMM is one pool
type, they've got two pool types on paper but they effectively have three. They
fall in two main categories, (1) Concentrated liquidity (CLMM) and (2) Constant
Product liquidity (CPMM/CP-Swap, AMM v4 Legacy).

CPMM is your standard, run of the mill AMM. XY = K type of deal, nothing to
write home about. Raydium apparently had a point in its history where liquidity
pools spilled over into an order book, previously it was Serum, as of late it is
OpenBook, and as of the late late, it's neither. They're encouraging you to
create CP-Swap pools. I haven't looked into
[CLMM yet but I know I definitely don't want to for now, dealing with this](https://github.com/raydium-io/raydium-clmm/blob/master/client/src/main.rs).

Yeah, no. Higher surface area than our friendly neighborhood
[Constant Product liquidity pools](https://github.com/raydium-io/raydium-cp-swap/blob/master/client/src/main.rs).

In anycase, Serum is dead now I think, so they moved off of that. Openbook was a
fork of Serum, V2 is the new black and I think it still powers Jupiter. The
thing about Raydium AMM v4 is, it's treated as legacy but it's still in active
use today. You can definitely create an AMM v4 pool today through their new V3
beta UI. V2 UI crashes for me, so I'm not sure what to make of that.

The one gripe I have with this whole thing is lack of access. I can't filter the
UI to show me the pools under the new CP-Swap program. Not a big problem but I
can already see testing this will be a pain in the neck. I've already created a
[pool for my testing](https://solscan.io/account/3ELLbDZkimZSpnWoWVAfDzeG24yi2LC4sB35ttfNCoEi)
purposes. `SOL` + `COPE`, no idea what `COPE` is but it seemed fitting, also
cheap.

I can and have already tested out calling
[getProgramAccounts](https://solana.com/docs/rpc/http/getprogramaccounts).
Interestingly, the sense I get from working with Solana is the general narrative
around bitpacking with a deterministic binary layout, store encoded, query
encoded, and only decide client side when you need to see something.

Filtering the accounts on through `getProgramAccounts` is actually comparing
bytes from a specific offset + a length. Which means I really need to know the
layout of the state, luckily again, I can see this in the
[CP-Swap program's code](https://github.com/raydium-io/raydium-cp-swap/blob/master/programs/cp-swap/src/states/pool.rs#L132).

Can't imagine how I'd need to approach this when interfacing with a protocol I
don't have access to its code, but if I have time I'll definitely give it a
shot. Pump.fun I'm looking at you.

Fun times.

## The plan

This is going to be a living documentation, in case it hasn't been obvious yet.
Eventually replaced with something meaningful and fit for a README but I like to
keep notes somewhere when I start on a new thing, you never know what future me
in 6 month knows, or lacks.

So, let's do it.

> although I was hoping I might manage to cheat my way into a happy weekend and
> see if their APIs are actually open source, alas, they're not. Using my brain
> then.

- [x] IDL ~= ABI, it seems I need to pack and unpack the bits in correct order -
      pun intended - luckily someone else did the hard work and put together
      anchor-go which can take an IDL and generate a Go package for us. I can
      get the IDL from `anchor idl fetch` and we're home sailing.
      [Raydium also publishes their IDL](https://github.com/raydium-io/raydium-idl).
      But we'll just generate it.
- [x] Then I reckon I want to fetch details about my pool. Seems fitting because
      I know what to expect, we'll do just that.
- [x] Quoting math without fees, explain the math in the code. Will also look
      into the [SDK](https://github.com/raydium-io/raydium-sdk-V2) and see if
      there's anything special going on there. There's also a demo repo and
      apparently a nicely documented
      [CPI repo](https://github.com/raydium-io/raydium-cpi). Might prove useful.
- [x] Validate intents and intent effects on the pool
- [x] Fix handling decimal points on input
- [x] Fix scaling the incorrect token balances
- [x] Improve our intent row and balances row display in the table, also surface
      errors inline
- [x] Fees, fees, fees.
- [ ] Make a trade. I have no idea how this will work out, I've been looking
      over CP-Swap's source code and I see two swap instructions, one
      `swap_base_input` and one `swap_base_output`. The what, where, and why is
      unclear. I'll ask Preplexity for some pointers later. There's enough
      examples of swap instructions though, an abundance really, which is
      lovely.
- [ ] Pull in all the CP-Swap pools for a pair and filter through that instead
      of providing the pool address directly.
- [ ] I'd like to deal with tickers instead of addresses, there doesn't seem to
      be a clear easy way to do so, but some references to metaplex here and
      there, after some digging, I landed on
      [a series of issues relevant to metadata](https://github.com/gagliardetto/solana-go/issues?q=is%3Aissue%20token%20metadata).
      Not sure if any would be useful but will cross that bridge when we get to
      it.
- [ ] Trading REPL
- [ ] I can already see calls like `getProgramAccounts` being a problem, can I
      cache it? When do I invalidate the cache? I think I can update it with a
      diff if I calculate the offset at which I last cached. Some bit math for a
      fun afternoon.

## Sources

I found these sources to be useful in the early hours of looking into things,
dropping them here for future reference:

- https://www.youtube.com/watch?v=NGGzw3pzwfY How to call any Solana Program
  (Solandy)
- https://docs.chainstack.com/reference/solana-getting-started I'm using
  chainstack as my RPC node provider, for no real reason other than I saw it, it
  didn't put me off too much, decided to try it.
- https://drpc.org/docs/solana-api I found their documentation on par with
  Solan's official docs
- https://solana.com/docs/rpc Solan's official docs, extremely extensive.
- https://solana.com/developers/cookbook/transactions/calculate-cost The cook
  book is actually very low signal, but there are some nuggets in it. This one
  in particular, now I'm quite familiar with Viem's simulation of transactions,
  this looks similar, might be useful.
- https://solana.com/developers/evm-to-svm Highest signal IMO from the docs. If
  not shallow in many areas, it's a good entry point
- https://github.com/raydium-io There's a lot to unpack here
