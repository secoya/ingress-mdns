# Ingress Frontend Zeroconf

This tool is mainly intended for minikube setups. It watches the kubernetes API
for any ingresses and broadcasts the hostnames in their rule spec
to the interface that connects minikube to your host machine.

## Install

`skaffold deploy`
