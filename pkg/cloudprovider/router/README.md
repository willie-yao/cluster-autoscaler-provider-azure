# Router Package

Package router provides a centralized way to include the Azure cloud provider in the Cluster Autoscaler binary.

This package registers Azure by default, without Go build tags.

## Note for forks and specialized builds

External forks or specialized deployments can bypass this package and instead use blank imports of the cloud provider packages directly in their main entry point.
