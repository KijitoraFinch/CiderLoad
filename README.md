# CiderLoad

[![License: MIT OR MIT-SUSHI-WARE](assets/license.svg)](LICENSE)

A Homebrew accelerator that uses parallel downloads with Range requests.

## Compatibility

Tested with **Homebrew 7.0.11 on macOS (Apple Silicon)**. 

## Installation

Using Homebrew:

```sh
brew tap KijitoraFinch/CiderLoad https://github.com/KijitoraFinch/CiderLoad.git
brew install kijitorafinch/ciderload/ciderload
```

## Usage

Install or upgrade formulae and Casks as usual:

```sh
cidl install llvm
cidl install --cask firefox
cidl upgrade
```

To check your installed CiderLoad version:

```sh
ciderload --ciderload-version
```

## Benchmarks (brew fetch)

Apple M5, 24 GiB RAM, macOS (2026-11-04). Median command runtime over 3 runs with an empty package cache.

| Task | Standard Homebrew (curl) | CiderLoad | Time reduction |
|---|---:|---:|---:|
| LLVM 23.1.4 + 4 dependencies | 186.56 s | 36.61 s | 80.4% |
| Go 1.27.3 | 14.36 s | 8.88 s | 38.2% |
| TeX Live 20260301_3 | 504.71 s | 159.33 s | 68.4% |
| Firefox 159.0.1 (Cask) | 46.90 s | 21.07 s | 55.1% |
| Docker Desktop 4.98.0 (Cask) | 34.77 s | 36.71 s | −5.6% |
| VS Code 1.144.1 (Cask) | 16.86 s | 13.66 s | 19.0% |

## By the way, what is this?
CiderLoad aims to speed up Homebrew downloads by using Range requests to download individual files in parallel when they are large enough. Standard Homebrew only downloads in parallel at the package level, so this greatly improves download speeds, especially for Casks whose servers impose strict speed limits per connection, or when downloading a single very large package. (GHCR, which hosts bottles, also appears to impose this kind of speed limits, so substantial speed improvements seem possible there as well.)
