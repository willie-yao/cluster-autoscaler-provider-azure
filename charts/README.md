# charts

The Helm chart for the Cluster Autoscaler project resides within this folder. If making changes to the Helm charts, make sure you follow the instructions below for the pre-commit checks.

## Pre-commit hooks

This Helm repository has pre-commit hooks for Helm specific needs:

* Makes sure all charts pass a `helm lint` check.

### Install `pre-commit` binary

The binary for `pre-commit` can be installed via Homebrew:

```shell
$ brew install pre-commit
```

For those without Homebrew, Pre-commit has [other installation methods available](https://pre-commit.com/#install).

### Install git hooks

After the `pre-commit` binary is installed, go to this repository's directory, and run the following command to install the git hook:

```shell
$ pre-commit install
```

### Install hook dependencies

The chart hook requires `helm` to be installed and available on `PATH`.
