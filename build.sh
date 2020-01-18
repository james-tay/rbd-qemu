#!/bin/sh

GOPATH="`pwd`/go"
export GOPATH

TARGET="terraform-provider-rbdqemu"

DEPS="
  github.com/hashicorp/terraform-plugin-sdk/plugin
  github.com/hashicorp/terraform-plugin-sdk/terraform
  github.com/hashicorp/terraform-plugin-sdk/helper/schema
"

case $1 in
'init')
  for I in $DEPS ; do
    echo "Adding dependency $I ..."
    go get -v $I || exit $?
  done
  ;;
*)
  go build -v -o $TARGET
  ;;
esac
