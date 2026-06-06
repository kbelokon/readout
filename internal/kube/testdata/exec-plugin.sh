#!/bin/bash -e
>&2 echo "$KUBERNETES_EXEC_INFO"
echo "$TEST_OUTPUT"
exit "${TEST_EXIT_CODE:-0}"
