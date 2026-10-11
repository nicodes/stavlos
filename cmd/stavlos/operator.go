package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nicodes/stavlos/internal/tools"
)

func cmdOperator(ctx context.Context, _ string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("operator reads one fixed grant request from standard input")
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 4097))
	if err != nil {
		return err
	}
	result := tools.RunOperator(ctx, input)
	if result.IsError {
		return fmt.Errorf("%s", result.Output)
	}
	fmt.Println(result.Output)
	return nil
}
