# vlcro

A parallel frontend for zypper. Speeds up slow zypper operations by running them in parallel across chroot-isolated workers


inspired by [zypperoni](https://github.com/pavinjosdev/zypperoni)


## Build

```
go build -o vlcro .
```

To install copy to path

## Usage

The command syntax is kept as similar as possible with zypper
```
usage: vlcro [-h] [-v] [-y] [-j N] [--debug] [--no-color]
      {refresh,ref,dist-upgrade,dup,update,up,install,in,install-new-recommends,inr,search,se} ...

vlcro (v0.1.0) makes zypper faster by running slow operations in parallel.

options:
  -h, --help            show this help message and exit
  -v, -V, --version     print version number and exit
  -y, --no-confirm      automatic yes to prompts, run non-interactively
  -j, --jobs            number of parallel operations (1-64, default: 10)
  --debug               enable debug output
  --no-color            disable color output

commands:
  refresh (ref)         refresh all enabled repos
    -f, --force         force a complete refresh

  dist-upgrade (dup)   perform distribution upgrade
    -d, --download-only download packages without installing

  update (up)         update all installed packages
    -d, --download-only download packages without installing

  search (se)          search for packages matching pattern
    <pattern>           pattern(s) to search for

  install (in)         install one or more packages
    -d, --download-only download packages without installing
    <package>           package name(s) to install

  install-new-recommends (inr)
                        install new packages recommended by already installed ones
    -d, --download-only download packages without installing

```

Downloads are handled in parallel; the actual install is handed back to zypper.
