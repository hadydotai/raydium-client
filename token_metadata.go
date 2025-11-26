package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

/*
NOTE(@hadydotai):

== Preamble

This is a rabbit hole, mostly because of the scattered documentation. What we're interested in
is finding out the token name and symbol. To get those, we have two areas to explore:

1. Pre Token-2022 extension (metadata living in metaplex account)
2. Post Token-2022 extension (metadata in proximity to the token account)

They're not exactly temporal, as in, it's not like the entire ecosystem has shifted gears towards the new SPL token
program with the Token-2022 extension. Seems to me at least, a lot of tokens are still being minted through metaplex
and so end up with their metadata living on a metaplex PDA.

Token-2022 came with a bunch of extensions, the two we're interested in are MetadataPointer and TokenMetadata. The complexity
is in how SPL-Token program, SPL-Token-2022 extensions, and the two account types AccountType::Mint and AccountType::Account
sort of overlap and store data on the chain.

Let's first untangle the Metaplex bits (pun intended) and then talk about Token-2022. Metaplex turned out to be the simpler
of those two locations. Figures.

=== Metaplex

The most valuable sources of information I've looked into have been has been these:
https://docs.rs/mpl-token-metadata/2.0.0-beta.1/mpl_token_metadata/state/struct.Metadata.html
https://docs.rs/mpl-token-metadata/2.0.0-beta.1/mpl_token_metadata/state/struct.Data.html
https://docs.rs/mpl-token-metadata/2.0.0/mpl_token_metadata/accounts/metadata/struct.Metadata.html

The data layout before v2 stored the name and symbol under a `Data` struct, after v2 beta.1 they flattend it.
It doesn't matter so much for us as much as the offset of the data and their size, both the size and order hasn't changed,
under `Data` struct, `name` came first then the `symbol`. When they flattened it, they retained the same order, and the size hasn't
changed because the data type for both hasn't changed, they're both Borsh string encoded. The offset hasn't changed either
because they still sit right after a Pubkey.

This is great, so all we need to do in order to get metadata out of this is obviously we first check the owner of the program, if it's
your standard SPL Token program, then we know it can't carry extensions, we compute a PDA for the Metaplex account and that will carry
our metadata. Fetch, decode, and move on.


=== Token-2022

Things get a little hairy here. Token-2022 is backward compatiable, it lays out its structure in two ways.

- AccountType::Mint: Is a standard token type, goes up to 82 bytes in length
- AccountType::Account: Is an account token holding tokens? Goes up to 165 bytes in length

On chain, you can't tell them apart. They share the same owner (Token-2022), they have the same starting byte order, no
embedded discriminator, and the only difference between them is length. In fact, writing this now, I think the AccountType::Mint in some
cases might zero pad 83 bytes to bring itself up to 165 bytes. I don't know under what conditions does this happen. It's documented in one location as
far as I can tell:
https://github.com/solana-program/token-2022/blob/main/interface/src/extension/mod.rs#L278-L297

and for account length we can find this here:
https://github.com/solana-program/token-2022/blob/main/interface/src/state.rs#L147

Note the account structure a few lines above it, yeah, now check this mint structure: https://github.com/solana-program/token-2022/blob/main/interface/src/state.rs#L28

In my scanning here, I'm basically just trying both out, if I start hitting some data then happy days, if not I try another layout.
The `extension/mod.rs` is a 3k lines file, I strongly urge you to read it, I had to digest the majority of the contract code for both Token-2022 and SPL Token programs, can't
say I understand the majority of it, but I get the relevant bits. Or rather, I know where to find something.

Anyway, now the layout. Data is packed in a TLV format, type, length, value. We read the type first and determine if this is an extension we're interested in,
these extension types are u16 bit fields that follow this enum:
https://github.com/solana-program/token-2022/blob/main/interface/src/extension/mod.rs#L1056

The length confused the hell out of me, in some areas online it appears to be a u32 4 byte field, in other areas it appears to be u16. So many conflicting accounts,
that I ended up relying exclusively on the source code of both the client and the interface for token-2022 and the implementation they have
for tlv here (https://github.com/solana-program/libraries/tree/main/type-length-value) to try and piece it together.

After some trial and error. I eventually discovered it's a 2 byte u16 value. The next bit, the data slab is actually reading up to the length
we just decoded and then parsing their values out.

These values are Borsh encoded structures which is fairly straight forward to work out.

The two extensions we care about have their layouts here:
https://github.com/solana-program/token-metadata/blob/main/interface/src/state.rs#L23
https://github.com/solana-program/token-2022/blob/main/interface/src/extension/metadata_pointer/mod.rs#L17

This is all well and lovely until you realize that some metadata don't live on the mint account directly, they have a pointer extension which
points at another account. In some cases, that account might be our mint account. In other cases, it might be another program that implements the same
interface, in either case they will both be TLV encoded. We follow the pointer and decode.

== Security note

So one interesting thing here is, we only follow the pointer one hop. I don't know if it's not explicitly supported to embed another redirect, because
you can really encode anything you want on the TLV value slab, what keeps me from minting an SPL-Token-2022 token, with a Metadata Pointer extension that
points to a program that implements the interface, but continues to point outwards, you know ****void style of shit. Except infinite. Sounds like a DoS waiting
to happen. In any case, we only follow one link. Also, yeah, on that point. I can point to myself. I saw this one token that had the pointer extension enabled
and it pointed at its mint address. So I guess it's good to just follow the jump only once.


== Layout

Alright so, preamble yapped out. Until I give a face lift to the code, these are the layouts we're working with:

1) Metaplex metadata account
   +----------------------+----------------------+------------------------+
   | key (1B)             | update_authority (32)| mint (32)              |
   +----------------------+----------------------+------------------------+
   | name (borsh string)  | symbol (borsh string)| uri (borsh string)     |
   +----------------------+----------------------+------------------------+
   | additional metadata: repeated (borsh string key, borsh string value) |
   +----------------------------------------------------------------------+

2) Token-2022 mint with canonical padding (82B mint promoted to 165B):
   +----------------+--------------------------+-----------------+----------------------+
   | Mint struct    | zero padding (83B)       | AccountType (1) | TLV entries…         |
   | (82 bytes)     |                          | value = 1 (Mint)|                      |
   +----------------+--------------------------+-----------------+----------------------+
                                                       			 TLV entry:
                                                       			 +-----------+------------+---------------+
                                                       			 | Type u16  | Length u16 | Value [length]|
                                                       			 +-----------+------------+---------------+

3) Token-2022 mint without padding:
   +----------------+-----------------+----------------------+
   | Mint struct    | AccountType (1) | TLV entries…         |
   | (82 bytes)     | value = 1       |                      |
   +----------------+-----------------+----------------------+

4) Extensions:
   a) MetadataPointer (fixed 64B payload)
      +------------------------------+
      | authority OptionalNonZeroPk  |
	  +------------------------------+
	  | metadata_address OptionalPk  |
      +------------------------------+

   b) TokenMetadata (variable-length Borsh)
      +----------------------------+
      | update_authority (32B)	   |
	  +----------------------------+
	  | mint (32B, must match)     |
      +----------------------------+
      | name (borsh string)        |
	  +----------------------------+
	  | symbol (borsh string)      |
      +----------------------------+
      | uri (borsh string)         |
	  +----------------------------+
	  | additional metadata vec    |
      +----------------------------+
      additional metadata vec = u32 length + that many (key,string value) pairs

- `token2022TLVRegion`
	we'll first attempt scanning for layout (2) by expecting 83 zero bytes plus AccountType before TLVs
	when that fails, we fall back to layout (3) and expect the very next byte to be AccountType::Mint.
- `parseToken2022TLVEntries`
	iterates TLVs where each header is `u16 type + u16 length` and value bytes, we'll short circuit
	when we find type 19 (TokenMetadata) or type 18 (MetadataPointer)
- `decodeMetadataPointer`
	consumes the MetadataPointer payload (4.a)
- `decodeToken2022MetadataEntry`
	consumes the TokenMetadata payload (4.b)

*/

var (
	// https://github.com/metaplex-foundation/mpl-token-metadata/blob/main/programs/token-metadata/program/src/lib.rs#L25
	// Took a minute to find this, it's also a badge on the README (Am I the only one who ignores these?)
	// devnet+mainnet
	MPLTokenMetaDataProgramID = solana.MustPublicKeyFromBase58("metaqbxxUerdq28cj1RbAWkYQm3ybzjb6a8bt518x1s")
	errTokenMetadataMissing   = errors.New("no Token-2022 TokenMetadata found")
)

// binaryReader is an abstraction over a collection of bytes with various reading and decoding methods
// it has no coherent structure or purpose, with no intention of being reusable, it's only there for
// convenience.
type binaryReader struct {
	b []byte
	i int
}

func (r *binaryReader) le16() (uint16, bool) {
	if r.i+2 > len(r.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint16(r.b[r.i : r.i+2])
	r.i += 2
	return v, true
}

func (r *binaryReader) le32() (uint32, bool) {
	if r.i+4 > len(r.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(r.b[r.i : r.i+4])
	r.i += 4
	return v, true
}

func (r *binaryReader) le32Len() (int, bool) {
	if r.i+4 > len(r.b) {
		return 0, false
	}
	n := binary.LittleEndian.Uint32(r.b[r.i : r.i+4])
	r.i += 4
	if n > uint32(len(r.b)-r.i) {
		return 0, false
	}
	return int(n), true
}

func (r *binaryReader) bytes(n int) ([]byte, bool) {
	if n < 0 || r.i+n > len(r.b) {
		return nil, false
	}
	v := r.b[r.i : r.i+n]
	r.i += n
	return v, true
}

func (r *binaryReader) remaining() int {
	return len(r.b) - r.i
}

// NOTE(@hadydotai): Reminder, borsh strings are u32 little endian length + raw bytes (NOT null-terminated), because you know
// they're length tagged/prefixed. I was originally searching for a null terminator AND parsing the length tag. Smart.
func (r *binaryReader) borshString() (string, bool) {
	n, ok := r.le32Len()
	if !ok {
		return "", false
	}
	s := string(r.b[r.i : r.i+n])
	r.i += n
	return s, true
}

func trimMeta(s string) string {
	return strings.TrimSpace(strings.TrimRight(s, "\x00"))
}

type Token struct {
	Name   string
	Symbol string
}

const (
	baseMintLen                  = 82
	baseAccountLen               = 165
	mintExtensionPaddingBytes    = baseAccountLen - baseMintLen
	accountTypeMint              = 1
	extensionTypeUninitialized   = 0
	extensionTypeMetadataPointer = 18
	extensionTypeTokenMetadata   = 19
)

func parseToken2022Metadata(ctx context.Context, client *rpc.Client, mint solana.PublicKey, data []byte) (Token, error) {
	if len(data) <= baseMintLen {
		return Token{}, errors.New("mint does not belong to a Token2022 token")
	}

	tlvRegion, err := token2022TLVRegion(data)
	if err != nil {
		return Token{}, err
	}

	token, pointer, err := parseToken2022TLVEntries(tlvRegion, mint)
	if err == nil {
		return token, nil
	}
	if pointer != nil {
		return fetchToken2022MetadataViaPointer(ctx, client, *pointer, mint)
	}
	return Token{}, err
}

func token2022TLVRegion(data []byte) ([]byte, error) {
	rest := data[baseMintLen:]
	if len(rest) == 0 {
		return nil, errors.New("token2022 mint missing extension bytes")
	}

	if len(rest) >= mintExtensionPaddingBytes+1 {
		padding := rest[:mintExtensionPaddingBytes]
		accountTypeMarker := rest[mintExtensionPaddingBytes]
		if allZero(padding) && accountTypeMarker == accountTypeMint {
			return rest[mintExtensionPaddingBytes+1:], nil
		}
	}

	if rest[0] != accountTypeMint {
		return nil, errors.New("token2022 mint missing account type marker")
	}
	return rest[1:], nil
}

func parseToken2022TLVEntries(tlv []byte, expectedMint solana.PublicKey) (Token, *solana.PublicKey, error) {
	r := binaryReader{b: tlv}
	var pointer solana.PublicKey
	pointerSet := false

	for {
		if r.remaining() == 0 {
			break
		}
		if r.remaining() < 4 {
			return Token{}, nil, fmt.Errorf("malformed token2022 TLV: truncated header (%d bytes remain)", r.remaining())
		}
		typ, ok := r.le16()
		if !ok {
			return Token{}, nil, fmt.Errorf("malformed token2022 TLV: unable to read type")
		}
		if typ == extensionTypeUninitialized {
			break
		}
		length, ok := r.le16()
		if !ok {
			return Token{}, nil, fmt.Errorf("malformed token2022 TLV: unable to read length")
		}
		if int(length) > r.remaining() {
			return Token{}, nil, fmt.Errorf("malformed token2022 TLV: length %d exceeds remaining %d", length, r.remaining())
		}
		value, ok := r.bytes(int(length))
		if !ok {
			return Token{}, nil, fmt.Errorf("malformed token2022 TLV: unable to read value")
		}

		switch typ {
		case extensionTypeTokenMetadata:
			token, err := decodeToken2022MetadataEntry(value, expectedMint)
			if err != nil {
				return Token{}, nil, err
			}
			return token, nil, nil
		case extensionTypeMetadataPointer:
			if pk, ok := decodeMetadataPointer(value); ok {
				pointer = pk
				pointerSet = true
			}
		default:
			// ignore unknown extensions
		}
	}

	if pointerSet {
		return Token{}, &pointer, nil
	}
	return Token{}, nil, errTokenMetadataMissing
}

func fetchToken2022MetadataViaPointer(ctx context.Context, client *rpc.Client, pointer solana.PublicKey, mint solana.PublicKey) (Token, error) {
	res, err := client.GetAccountInfoWithOpts(ctx, pointer, &rpc.GetAccountInfoOpts{
		Encoding:   solana.EncodingBase64,
		Commitment: rpc.CommitmentProcessed,
	})
	if err != nil {
		return Token{}, fmt.Errorf("rpc call getAccountInfo failed for metadata pointer %s: %w", Addr(pointer.String()), err)
	}
	if res.Value == nil {
		return Token{}, fmt.Errorf("account data empty for metadata pointer %s", Addr(pointer.String()))
	}
	buf := res.Value.Data.GetBinary()
	if len(buf) == 0 {
		return Token{}, fmt.Errorf("metadata pointer %s has empty data", Addr(pointer.String()))
	}
	// NOTE(@hadydotai): While it's entirely possible we might get another hop, I don't know if it's actually supported or not
	// so I'm just ignore it here and simply relying on the notion that if it's not on the mint/account, and we had a pointer
	// then that's the first and last hop we're taking.
	//
	// I actually wonder if this is really possible, because it feels like a glaring attack vector. A very easy way to DoS a dApp
	token, _, err := parseToken2022TLVEntries(buf, mint)
	if err == nil {
		return token, nil
	}

	if err != errTokenMetadataMissing {
		return Token{}, err
	}

	token, err = decodeToken2022MetadataEntry(buf, mint)
	if err != nil {
		return Token{}, fmt.Errorf("failed decoding metadata via pointer %s: %w", Addr(pointer.String()), err)
	}
	return token, nil
}

func decodeToken2022MetadataEntry(val []byte, expectedMint solana.PublicKey) (Token, error) {
	r := &binaryReader{b: val}
	if _, ok := r.bytes(32); !ok {
		return Token{}, errors.New("invalid token metadata: update authority missing")
	}
	mintBytes, ok := r.bytes(32)
	if !ok {
		return Token{}, errors.New("invalid token metadata: mint missing")
	}
	if !equal32(mintBytes, expectedMint.Bytes()) {
		return Token{}, errors.New("token metadata mint mismatch")
	}
	name, ok := r.borshString()
	if !ok {
		return Token{}, errors.New("invalid token metadata: name missing")
	}
	symbol, ok := r.borshString()
	if !ok {
		return Token{}, errors.New("invalid token metadata: symbol missing")
	}
	if _, ok := r.borshString(); !ok {
		return Token{}, errors.New("invalid token metadata: uri missing")
	}
	additionalCount, ok := r.le32()
	if !ok {
		return Token{}, errors.New("invalid token metadata: additional metadata length missing")
	}
	for i := uint32(0); i < additionalCount; i++ {
		if _, ok := r.borshString(); !ok {
			return Token{}, errors.New("invalid token metadata: additional metadata key missing")
		}
		if _, ok := r.borshString(); !ok {
			return Token{}, errors.New("invalid token metadata: additional metadata value missing")
		}
	}
	return Token{Name: trimMeta(name), Symbol: trimMeta(symbol)}, nil
}

func decodeMetadataPointer(val []byte) (solana.PublicKey, bool) {
	if len(val) < 64 {
		return solana.PublicKey{}, false
	}
	metaBytes := val[32:64]
	pk := solana.PublicKeyFromBytes(metaBytes)
	if isZeroPubkey(pk) {
		return solana.PublicKey{}, false
	}
	return pk, true
}

// func equal8(a, b []byte) bool {
// 	if len(a) != 8 || len(b) != 8 {
// 		return false
// 	}
// 	for i := 0; i < 8; i++ {
// 		if a[i] != b[i] {
// 			return false
// 		}
// 	}
// 	return true
// }

func equal32(a, b []byte) bool {
	if len(a) != 32 || len(b) != 32 {
		return false
	}
	for i := 0; i < 32; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func isZeroPubkey(pk solana.PublicKey) bool {
	zero := solana.PublicKey{}
	return pk.Equals(zero)
}

func parseMetaplexMetadata(ctx context.Context, client *rpc.Client, mint solana.PublicKey) (Token, error) {

	pda, _, err := solana.FindProgramAddress(
		[][]byte{
			[]byte("metadata"),
			MPLTokenMetaDataProgramID.Bytes(),
			mint.Bytes(),
		},
		MPLTokenMetaDataProgramID,
	)
	if err != nil {
		return Token{}, fmt.Errorf("error deriving PDA to get token metadata %s: %w", Addr(mint.String()), err)
	}

	res, err := client.GetAccountInfoWithOpts(ctx, pda, &rpc.GetAccountInfoOpts{
		Encoding:   solana.EncodingBase64,
		Commitment: rpc.CommitmentProcessed,
	})
	if err != nil {
		return Token{}, fmt.Errorf("rpc call getAccountInfo failed: %w", err)
	}
	if res.Value == nil {
		return Token{}, fmt.Errorf("account data empty for mint %s", Addr(mint.String()))
	}
	if res.Value.Owner != MPLTokenMetaDataProgramID {
		return Token{}, fmt.Errorf("account %s not owned by mpl-token-metadata (owner=%s)", Addr(mint.String()), Addr(res.Value.Owner.String()))
	}

	r := &binaryReader{b: res.Value.Data.GetBinary()}

	if _, ok := r.bytes(1); !ok { // key
		return Token{}, errors.New("failed skipping token key")
	}
	if _, ok := r.bytes(32); !ok { // update_authority
		return Token{}, errors.New("failed skipping token update authority")
	}
	if _, ok := r.bytes(32); !ok { // mint
		return Token{}, errors.New("failed skipping token mint")
	}

	// for v1-until-v2-beta1 this is Data.name
	// for v2.x it's a top-level field after mint.
	name, ok := r.borshString()
	if !ok {
		return Token{}, errors.New("failed parsing token name")
	}

	// for v1-until-v2-beta1 this is Data.symbol
	// for v2.x it's a top-level field after mint.
	symbol, ok := r.borshString()
	if !ok {
		return Token{}, errors.New("failed parsing token symbol")
	}

	return Token{Name: trimMeta(name), Symbol: trimMeta(symbol)}, nil
}

func tokenMetadata(ctx context.Context, client *rpc.Client, mint solana.PublicKey) (Token, error) {
	res, err := client.GetAccountInfoWithOpts(ctx, mint, &rpc.GetAccountInfoOpts{
		Encoding:   solana.EncodingBase64,
		Commitment: rpc.CommitmentProcessed,
	})
	if err != nil {
		return Token{}, fmt.Errorf("rpc call getAccountInfo failed: %w", err)
	}
	if res.Value == nil {
		return Token{}, fmt.Errorf("account data empty for mint %s", Addr(mint.String()))
	}
	data := res.Value.Data.GetBinary()
	owner := res.Value.Owner
	switch owner.String() {
	case solana.Token2022ProgramID.String():
		return parseToken2022Metadata(ctx, client, mint, data)
	case solana.TokenProgramID.String():
		return parseMetaplexMetadata(ctx, client, mint)
	}
	return Token{}, fmt.Errorf("couldn't get metadata for token %s", Addr(mint.String()))
}
