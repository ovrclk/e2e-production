FROM debian:buster

ENV E2E_GAS=auto
ENV E2E_HOME=/data/.akash
ENV E2E_KEYRING_BACKEND=test
ENV E2E_GAS_ADJUSTMENT=1.25
ENV E2E_GAS_PRICES=0.025uakt
ENV E2E_SIGN_MODE=amino-json

COPY .akash /data/.akash
COPY e2e /bin/

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && update-ca-certificates \
 && apt-get clean autoclean \
 && apt-get autoremove --yes \
 && rm -rf /var/lib/{apt,dpkg,cache,log}
