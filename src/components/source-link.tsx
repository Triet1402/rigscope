// SPDX-License-Identifier: AGPL-3.0-only
const version = process.env.NEXT_PUBLIC_RIGSCOPE_VERSION ?? "dev";
const upstream = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(version)
  ? `https://github.com/Triet1402/rigscope/releases/download/v${version}/rigscope_${version}_source.tar.gz`
  : "https://github.com/Triet1402/rigscope";
const source = process.env.NEXT_PUBLIC_RIGSCOPE_SOURCE_URL || upstream;

export function SourceLink() {
  return <a href={source} className="text-xs text-muted-foreground underline underline-offset-4 hover:text-foreground">Source code · AGPLv3</a>;
}
