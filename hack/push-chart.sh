#!/bin/bash -eu

helm=$1
chart_tarball=$2
chart_remote=$3
tag_file=$4
digest_file=$5

${helm} push "${chart_tarball}" "${chart_remote}" |& tee /dev/stderr |
  while read -r field value; do
    case "${field}" in
    Pushed:)
      echo "tag=${value}"
      echo -n "${value}" >"${tag_file}"
      ;;
    Digest:)
      echo "digest=${value}"
      echo -n "${value}" >"${digest_file}"
      ;;
    esac
  done
