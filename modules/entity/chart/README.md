# entity chart

The `entity` module is served by the same binary and release as `board` (`cmd/can-board`); its
`values.yaml` repeats the board module's contract and names it with `sharedRelease: board`. The Helm
templates arrive with the module deployment epic (E03). Until then both are validated by `can validate`
and run directly with `cmd/can-board`.
