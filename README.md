# E2E testing on production environment

[![equinix-metal-ewr1](https://github.com/ovrclk/e2e-production/actions/workflows/equinix-metal-ewr1.yaml/badge.svg)](https://github.com/ovrclk/e2e-production/actions/workflows/equinix-metal-ewr1.yaml)
[![equinix-metal-ams1](https://github.com/ovrclk/e2e-production/actions/workflows/equinix-metal-ams1.yaml/badge.svg)](https://github.com/ovrclk/e2e-production/actions/workflows/equinix-metal-ams1.yaml)
[![equinix-metal-sjc1](https://github.com/ovrclk/e2e-production/actions/workflows/equinix-metal-sjc1.yaml/badge.svg)](https://github.com/ovrclk/e2e-production/actions/workflows/equinix-metal-sjc1.yaml)

- uses modified version of akash deploy cmd
- runs on either push to this repo or by cron trigger every 15min
- [deploys](manifests/deployment.yaml) lunie-light to all available akash providers
    - ewr1-provider0
    - sjc1-provider0
- once deployed checks each lease's http endpoints to return 200
- when test finishes (regardless PASS or FAIL) deployment is automatically closed
