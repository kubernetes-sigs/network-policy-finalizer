# network-policy-finalizer

NetworkPolicyFinalizer prevents premature deletion of Kubernetes Network Policies by using a finalizer. This ensures that policies are retained until the pods that depend on them are removed, maintaining network security.

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack](https://kubernetes.slack.com/messages/sig-network)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/sig-network)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).
