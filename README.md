# E2E testing on production environment

![E2E Cron](https://github.com/ovrclk/e2e-production/actions/workflows/e2e.yaml/badge.svg?event=schedule)
![E2E Master](https://github.com/ovrclk/e2e-production/actions/workflows/e2e-master.yaml/badge.svg?event=push)
![Release](https://github.com/ovrclk/e2e-production/actions/workflows/release.yaml/badge.svg?event=push)

- uses modified version of akash deploy cmd
- runs on either push to this repo or by cron trigger every 15min
- [deploys](manifests/deployment.yaml) lunie-light to all available akash providers
    - ewr1-provider0
    - sjc1-provider0
- once deployed checks each lease's http endpoints to return 200
- when test finishes (regardless PASS or FAIL) deployment is automatically closed
