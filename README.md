# network-policy-finalizer

NetworkPolicyFinalizer prevents premature deletion of Kubernetes Network Policies by using a finalizer. This ensures that policies are retained until the pods that depend on them are removed, maintaining network security.

This will not be required after [KEP 5080: Ordered namespace deletion](https://github.com/kubernetes/enhancements/issues/5080) is implemented.

## How it works

The network-policy-finalizer is a controller that adds a finalizer to every NetworkPolicy object created and, when the NetworkPolicy is being deleted by the namespace controller, it waits for the Pods associated to the NetworkPolicy to be deleted before removing the finalizer.

## Install

```sh
kubectl apply -f https://github.com/kubernetes-sigs/network-policy-finalizer/blob/main/install.yaml
```

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack](https://kubernetes.slack.com/messages/sig-network)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/sig-network)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).
