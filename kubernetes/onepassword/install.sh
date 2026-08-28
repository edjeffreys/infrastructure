#!/bin/bash
# Run this once after creating the token secret.
# Assumes KUBECONFIG is set or you're using the cluster's kubeconfig.

set -e

KUBECONFIG=/etc/rancher/k3s/k3s.yaml

kubectl apply -f namespace.yaml
kubectl apply -f 1password-token-secret.yaml

helm repo add 1password https://1password.github.io/connect-helm-charts/
helm repo update

helm upgrade --install onepassword-operator 1password/onepassword-operator \
  --namespace onepassword-operator \
  --set operator.serviceAccount.token.credentials.serviceAccountToken="" \
  --set operator.serviceAccount.token.credentialsName=onepassword-service-account-token \
  --set operator.serviceAccount.token.credentialsKey=token \
  --wait
