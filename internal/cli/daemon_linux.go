package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// starttime 在进程生命周期内不变。僵尸已经退出，回收它是父进程的职责；
// 同时支持尚无 pidfd 的内核，等待方无需是 daemon 的父进程。
func observeExit(pid int) (exitProbe, error) {
	path := "/proc/" + strconv.Itoa(pid) + "/stat"
	sample := func() (string, bool, error) {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return "", true, nil
		}
		if err != nil {
			return "", false, err
		}
		// comm 可以含空格和括号；最后一个右括号之后才是固定位置的状态字段。
		i := strings.LastIndexByte(string(raw), ')')
		fields := strings.Fields(string(raw[i+1:]))
		if i < 0 || len(fields) < 20 {
			return "", false, fmt.Errorf("进程状态字段不完整")
		}
		return fields[19], fields[0] == "Z" || fields[0] == "X", nil
	}
	start, _, err := sample()
	if err != nil {
		return exitProbe{}, err
	}
	exited := func() (bool, error) {
		now, dead, err := sample()
		return dead || now != start, err
	}
	return exitProbe{exited, func() error { return nil }}, nil
}
