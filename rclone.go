package main

import (
	_ "github.com/poplicola/rclone-remarkable/backend/remarkable"
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/all"
)

func main() {
	cmd.Main()
}
