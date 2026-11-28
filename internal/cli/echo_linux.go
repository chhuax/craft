package cli

import "syscall"

const getTermios = syscall.TCGETS
const setTermios = syscall.TCSETS
