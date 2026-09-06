# vlcro

WIP README

A parallel frontend for zypper. Speeds up slow zypper operations by running them in parallel across chroot-isolated workers


inspirated by [zypperoni](https://github.com/pavinjosdev/zypperoni)


## Build

```
go build -o vlcro .
```

To install copy to path

## Usage

```
sudo vlcro <command> [options]
```

Downloads are handled in parallel; the actual install is handed back to zypper.
