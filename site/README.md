# RoamVM website

Static HTML, CSS, and JavaScript. No build step or external requests are required.
The Inter font is self-hosted; its OFL license is in `assets/Inter-LICENSE.txt`.
Design tokens are defined in `styles.css` under `:root`.
`assets/social.png` is a 1200×630 browser rendering of `assets/social.svg`; render
it again after changing the share-card artwork, with Inter loaded before capture.

```sh
python3 -m http.server 8080 --bind 127.0.0.1 --directory site
```

Open http://127.0.0.1:8080. Check the lifecycle controls, YAML copy button,
disclosures, keyboard focus, and layouts at mobile and desktop widths.

The Pages workflow publishes this directory when its files change on `main`.
Enable GitHub Actions as the repository's Pages source. The public address is
https://dialohq.github.io/roamvm/; assets use relative paths to support that prefix.

Documentation links point to the tested runtime revision while its integration
PR is open. Update them when documenting a newer runtime revision.
