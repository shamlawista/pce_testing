// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package main

import (
	"encoding/json"
	"errors"
	"fmt"

	pb "github.com/nttcom/pola/api/pola/v1"
	"github.com/nttcom/pola/cmd/pola/grpc"
	"github.com/spf13/cobra"
)

// newNodeExcludeCmd is the parent for managing the global node-exclusion
// set: router IDs kept out of CSPF consideration for every dynamically-
// computed policy server-wide, on top of each policy's own `exclude` (see
// `pola sr-policy add`'s `exclude` field for the per-policy equivalent).
func newNodeExcludeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "node-exclude",
		RunE: func(cmd *cobra.Command, args []string) error {
			return showExcludedNodes(jsonFmt)
		},
	}

	cmd.AddCommand(newNodeExcludeAddCmd(), newNodeExcludeRemoveCmd(), newNodeExcludeListCmd())
	return cmd
}

// nodeArgFlags reads --routerID/--sid off cmd and validates exactly one is
// set, mirroring the same validation the server applies (see
// resolveExcludedNodeArg in pkg/server/grpc_server.go).
func nodeArgFlags(cmd *cobra.Command) (routerID, sid string, err error) {
	routerID, err = cmd.Flags().GetString("routerID")
	if err != nil {
		return "", "", err
	}
	sid, err = cmd.Flags().GetString("sid")
	if err != nil {
		return "", "", err
	}
	if routerID == "" && sid == "" {
		return "", "", errors.New("exactly one of --routerID / --sid is required")
	}
	if routerID != "" && sid != "" {
		return "", "", errors.New("--routerID and --sid are mutually exclusive, use exactly one")
	}
	return routerID, sid, nil
}

func newNodeExcludeAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "add",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			routerID, sid, err := nodeArgFlags(cmd)
			if err != nil {
				return err
			}
			if err := grpc.AddExcludedNode(client, &pb.AddExcludedNodeRequest{RouterId: routerID, Sid: sid}); err != nil {
				return fmt.Errorf("failed to add excluded node: %v", err)
			}
			if jsonFmt {
				fmt.Printf("{\"status\": \"success\"}\n")
			} else {
				fmt.Printf("success!\n")
			}
			return nil
		},
	}
	cmd.Flags().String("routerID", "", "router ID to exclude")
	cmd.Flags().String("sid", "", "SID identifying the router to exclude, resolved against the TED")
	return cmd
}

func newNodeExcludeRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "remove",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			routerID, sid, err := nodeArgFlags(cmd)
			if err != nil {
				return err
			}
			if err := grpc.RemoveExcludedNode(client, &pb.RemoveExcludedNodeRequest{RouterId: routerID, Sid: sid}); err != nil {
				return fmt.Errorf("failed to remove excluded node: %v", err)
			}
			if jsonFmt {
				fmt.Printf("{\"status\": \"success\"}\n")
			} else {
				fmt.Printf("success!\n")
			}
			return nil
		},
	}
	cmd.Flags().String("routerID", "", "router ID to remove from the exclusion set")
	cmd.Flags().String("sid", "", "SID identifying the router to remove, resolved against the TED")
	return cmd
}

func newNodeExcludeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return showExcludedNodes(jsonFmt)
		},
	}
}

func showExcludedNodes(jsonFlag bool) error {
	routerIDs, err := grpc.GetExcludedNodes(client)
	if err != nil {
		return err
	}

	if jsonFlag {
		outputJSON, err := json.Marshal(routerIDs)
		if err != nil {
			return err
		}
		fmt.Println(string(outputJSON))
		return nil
	}

	if len(routerIDs) == 0 {
		fmt.Println("no globally excluded nodes")
		return nil
	}
	for _, id := range routerIDs {
		fmt.Println(id)
	}
	return nil
}
