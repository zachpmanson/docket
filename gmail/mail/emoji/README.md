# Pinned emoji sequence data

`fully-qualified-16.0.txt` contains exactly the 3,781 fully-qualified sequences
from Unicode's [Emoji 16.0 emoji-test.txt](https://unicode.org/Public/emoji/16.0/emoji-test.txt),
one literal UTF-8 sequence per line, in upstream order. No normalization or
variation-selector removal is performed. Source dated 2024-08-14; © 2024 Unicode,
Inc. Licensed under Unicode License V3 (included as `LICENSE`).

Source SHA-256:
`24f0c534e86cf142e2496953e8f0e46a3e702392911eddcd29c6cced85139697`.

Reproduce after downloading that public file and verifying its digest:

```sh
perl -CS -ne 'if (/^([0-9A-F ]+)\s*; fully-qualified\s*#/) { @cp=split / +/, $1; print join("",map { chr(hex($_)) } @cp),"\n"; }' emoji-test.txt > fully-qualified-16.0.txt
```

The 44,925-byte table is embedded by `../reaction_emoji.go`. Exact membership
accepts whole Unicode emoji sequences, not arbitrary graphemes or symbols.
Conservative scope excludes component-only, minimally-qualified/unqualified and
future emoji sequences. Upgrade requires deliberate data/version/test review;
there is no runtime download or new Go module dependency.
