// xrun is the client CLI for xrund daemon.
package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/microvm/sandbox/api/proto"
)

const defaultSocket = "/run/xrun/xrund.sock"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var socketPath string

	cmd := &cobra.Command{
		Use:   "xrun",
		Short: "Client for xrund - microVM sandbox manager",
	}

	cmd.PersistentFlags().StringVar(&socketPath, "socket", defaultSocket, "Path to xrund socket")

	cmd.AddCommand(newRunCmd(&socketPath))
	cmd.AddCommand(newStopCmd(&socketPath))
	cmd.AddCommand(newDeleteCmd(&socketPath))
	cmd.AddCommand(newListCmd(&socketPath))
	cmd.AddCommand(newGetCmd(&socketPath))
	cmd.AddCommand(newSnapshotCmd(&socketPath))
	cmd.AddCommand(newRestoreCmd(&socketPath))

	return cmd
}

func newClient(socketPath string) (pb.SandboxServiceClient, *grpc.ClientConn, error) {
	conn, err := grpc.Dial(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to xrund: %w", err)
	}

	return pb.NewSandboxServiceClient(conn), conn, nil
}

func newRunCmd(socketPath *string) *cobra.Command {
	var (
		image   string
		rootfs  string
		vcpus   uint32
		memory  uint32
		cmdline string
		labels  []string
	)

	cmd := &cobra.Command{
		Use:   "run [ID]",
		Short: "Create and start a new sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			client, conn, err := newClient(*socketPath)
			if err != nil {
				return err
			}
			defer conn.Close()

			labelMap := parseLabels(labels)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			resp, err := client.Run(ctx, &pb.RunRequest{
				Id:       id,
				Image:    image,
				Rootfs:   rootfs,
				Vcpus:    vcpus,
				MemoryMb: memory,
				Cmdline:  cmdline,
				Labels:   labelMap,
			})
			if err != nil {
				return fmt.Errorf("failed to run sandbox: %w", err)
			}

			fmt.Printf("Sandbox %s started\n", resp.Id)
			fmt.Printf("State: %s\n", resp.State)
			fmt.Printf("PID: %d\n", resp.Pid)
			if resp.IpAddress != "" {
				fmt.Printf("IP: %s\n", resp.IpAddress)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&image, "image", "i", "", "OCI image reference (required)")
	cmd.Flags().StringVarP(&rootfs, "rootfs", "r", "", "Rootfs disk path")
	cmd.Flags().Uint32VarP(&vcpus, "vcpus", "c", 1, "Number of vCPUs")
	cmd.Flags().Uint32VarP(&memory, "memory", "m", 512, "Memory in MB")
	cmd.Flags().StringVar(&cmdline, "cmdline", "", "Kernel command line (auto-configured if empty)")
	cmd.Flags().StringArrayVarP(&labels, "label", "l", nil, "Labels (key=value)")
	cmd.MarkFlagRequired("image")

	return cmd
}

func newStopCmd(socketPath *string) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "stop [ID]",
		Short: "Stop a running sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			client, conn, err := newClient(*socketPath)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := client.Stop(ctx, &pb.StopRequest{
				Id:    id,
				Force: force,
			})
			if err != nil {
				return fmt.Errorf("failed to stop sandbox: %w", err)
			}

			fmt.Printf("Sandbox %s stopped\n", resp.Id)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force stop")

	return cmd
}

func newDeleteCmd(socketPath *string) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "delete [ID]",
		Short: "Delete a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			client, conn, err := newClient(*socketPath)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			_, err = client.Delete(ctx, &pb.DeleteRequest{
				Id:    id,
				Force: force,
			})
			if err != nil {
				return fmt.Errorf("failed to delete sandbox: %w", err)
			}

			fmt.Printf("Sandbox %s deleted\n", id)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force delete")

	return cmd
}

func newListCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all sandboxes",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, err := newClient(*socketPath)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			resp, err := client.List(ctx, &pb.ListRequest{})
			if err != nil {
				return fmt.Errorf("failed to list sandboxes: %w", err)
			}

			if len(resp.Sandboxes) == 0 {
				fmt.Println("No sandboxes found")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "ID\tSTATE\tPID\tVCPUS\tMEMORY\tIMAGE\tIP")
			for _, sb := range resp.Sandboxes {
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%dMB\t%s\t%s\n",
					sb.Id, sb.State, sb.Pid, sb.Vcpus, sb.MemoryMb,
					sb.Image, sb.IpAddress)
			}
			w.Flush()

			return nil
		},
	}
}

func newGetCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "get [ID]",
		Short: "Get sandbox details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			client, conn, err := newClient(*socketPath)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			resp, err := client.Get(ctx, &pb.GetRequest{Id: id})
			if err != nil {
				return fmt.Errorf("failed to get sandbox: %w", err)
			}

			sb := resp.Sandbox
			fmt.Printf("ID: %s\n", sb.Id)
			fmt.Printf("State: %s\n", sb.State)
			fmt.Printf("PID: %d\n", sb.Pid)
			fmt.Printf("VCPUs: %d\n", sb.Vcpus)
			fmt.Printf("Memory: %dMB\n", sb.MemoryMb)
			fmt.Printf("Image: %s\n", sb.Image)
			fmt.Printf("Rootfs: %s\n", sb.Rootfs)
			fmt.Printf("IP: %s\n", sb.IpAddress)
			fmt.Printf("Created: %s\n", sb.CreatedAt)

			return nil
		},
	}
}

func newSnapshotCmd(socketPath *string) *cobra.Command {
	var name string

	cmd := &cobra.Command{
		Use:   "snapshot [ID]",
		Short: "Create a snapshot of a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			client, conn, err := newClient(*socketPath)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			resp, err := client.Snapshot(ctx, &pb.SnapshotRequest{
				Id:   id,
				Name: name,
			})
			if err != nil {
				return fmt.Errorf("failed to create snapshot: %w", err)
			}

			fmt.Printf("Snapshot created: %s\n", resp.SnapshotId)
			return nil
		},
	}

	cmd.Flags().StringVarP(&name, "name", "n", "", "Snapshot name (required)")
	cmd.MarkFlagRequired("name")

	return cmd
}

func newRestoreCmd(socketPath *string) *cobra.Command {
	var newID string

	cmd := &cobra.Command{
		Use:   "restore [SNAPSHOT_ID]",
		Short: "Restore a sandbox from a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			snapshotID := args[0]

			client, conn, err := newClient(*socketPath)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			resp, err := client.Restore(ctx, &pb.RestoreRequest{
				SnapshotId: snapshotID,
				NewId:      newID,
			})
			if err != nil {
				return fmt.Errorf("failed to restore sandbox: %w", err)
			}

			fmt.Printf("Sandbox restored: %s\n", resp.Id)
			return nil
		},
	}

	cmd.Flags().StringVar(&newID, "new-id", "", "New ID for restored sandbox (required)")
	cmd.MarkFlagRequired("new-id")

	return cmd
}

func parseLabels(labels []string) map[string]string {
	result := make(map[string]string)
	for _, label := range labels {
		parts := splitLabel(label)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

func splitLabel(label string) []string {
	for i, c := range label {
		if c == '=' {
			return []string{label[:i], label[i+1:]}
		}
	}
	return []string{label}
}
