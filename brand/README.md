# Brand assets (masters)

Source logo files for readout. The assets served by the app are **derived** from
these — edit a master here, then regenerate the served asset:

| Served asset                              | Derived from              | What it is                                  |
| ----------------------------------------- | ------------------------- | ------------------------------------------- |
| `internal/assets/static/favicon.png`      | `readout-icon.png`        | 64×64 PNG, dark tile, browser tab icon      |
| `internal/assets/static/readout-logo.svg` | `readout-icon.svg`        | transparent, cropped tight, navbar/header   |

## Masters

- `readout-icon.svg` — original vector master, 2048×2048, dark `#040510` background. The served `readout-logo.svg` is this with the background path dropped and the viewBox cropped tight to the mark.
- `readout-icon.png` — original raster, 2048×2048, dark `#040510` background.
- `readout-icon-transparent.png` — 902×902 raster, transparent background. Handy where a fixed-size transparent PNG is needed (social preview, README, external).

## Brand color

Green `#56E0BC`.

The navbar mark sits on a small dark chip (`#0d1117`) so the light-green stays
readable on both the dark and the light theme header (see `.brand-logo` in
`internal/assets/static/readout.css`).
