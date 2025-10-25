#!/bin/sh
k -n ai-chat  get events  --sort-by='.lastTimestamp'
