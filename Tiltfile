allow_k8s_contexts('microk8s')
k8s_yaml(kustomize('deploy/dev'))
docker_build('localhost:32000/dev/ingress-mdns', 'deploy/dev')
