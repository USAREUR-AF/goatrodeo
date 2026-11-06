<h1 align="center">goatrodeo</h1>

<p align="center">
  Games and toys that run on a goat data fabric. Each one exercises Zenoh in public.
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License: Apache 2.0"></a>
</p>

Everything here sits beside [`goatcore`](https://github.com/usareur-af/goatcore), which builds the goatnet, and [`goatstuff`](https://github.com/usareur-af/goatstuff), which holds the inspection surfaces. None of it is needed to run a goatnet. Each thing here publishes real events onto the fabric, so it doubles as a traffic generator you can watch in flashlight, panoscope or `goat sub`.

| Thing | What it is | Where |
|---|---|---|
| **airdefenders** | A co-op browser game. Eight auto-pilot bots can play it; every radar detection, missile launch, intercept and impact is published onto the goatnet's Zenoh fabric | server `ops/serve/airdefenders/`, schemas `ops/zenoh-trial-peers/airdefenders-schemas/`, role `ops/ansible/roles/airdefenders/`, playbooks `ops/ansible/airdefenders.yml` and `airdefenders-deploy.yml` |

## Run airdefenders on a laptop

```
cd ops/serve/airdefenders
go test ./...
docker compose up --build
```

Then open `http://localhost:8095/`. With no fabric configured the game runs on its own; the Zenoh publisher is off.

## Run it on a goatnet

The runbook is [`docs/operations/airdefenders-zenoh-demo.md`](docs/operations/airdefenders-zenoh-demo.md); the design is [`docs/design/airdefenders-multiplayer.md`](docs/design/airdefenders-multiplayer.md). The role and playbooks assume an assembled goatcore tree with this repository's `ops/` copied over it, the same way goatstuff's surfaces deploy.

The image is built by `.github/workflows/airdefenders-image.yml` and published as `ghcr.io/usareur-af/airdefenders`.

## Contributing

Pull requests are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues privately, as [SECURITY.md](SECURITY.md) describes.
