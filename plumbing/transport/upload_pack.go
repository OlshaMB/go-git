package transport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/storage"
)

// UploadPackOptions is a set of options for the UploadPack service.
type UploadPackOptions struct {
	GitProtocol   string
	AdvertiseRefs bool
	StatelessRPC  bool
}

// UploadPack is a server command that serves the upload-pack service.
func UploadPack(
	ctx context.Context,
	st storage.Storer,
	r io.ReadCloser,
	w io.WriteCloser,
	opts *UploadPackOptions,
) error {
	if r == nil || w == nil {
		return fmt.Errorf("nil reader or writer")
	}

	if opts == nil {
		opts = &UploadPackOptions{}
	}

	switch version := ProtocolVersion(opts.GitProtocol); version {
	case protocol.V2:
		// TODO: support version 2
	case protocol.V1:
		if _, err := pktline.Writef(w, "version=%s\n", version.String()); err != nil {
			return err
		}
		fallthrough
	case protocol.V0:
	default:
		return fmt.Errorf("unknown protocol version %q", version)
	}

	if opts.AdvertiseRefs || !opts.StatelessRPC {
		if err := AdvertiseReferences(ctx, st, w, UploadPackService, opts.StatelessRPC); err != nil {
			return fmt.Errorf("advertising references: %w", err)
		}
	}

	rd := bufio.NewReader(r)
	if !opts.AdvertiseRefs {
		l, _, err := pktline.PeekLine(rd)
		if err != nil {
			return fmt.Errorf("peeking line: %w", err)
		}

		// In case the client has nothing to send, it sends a flush packet to
		// indicate that it is done sending data. In that case, we're done
		// here.
		if l == pktline.Flush {
			return nil
		}

		// TODO: implement server negotiation algorithm
		// Receive upload request

		upreq := packp.NewUploadRequest()
		if err := upreq.Decode(rd); err != nil {
			return fmt.Errorf("decoding upload-request: %w", err)
		}

		if err := r.Close(); err != nil {
			return fmt.Errorf("closing reader: %w", err)
		}

		// TODO: support deepen-since, and deepen-not
		var shupd packp.ShallowUpdate
		if !upreq.Depth.IsZero() {
			switch depth := upreq.Depth.(type) {
			case packp.DepthCommits:
				if err := getShallowCommits(st, upreq.Wants, int(depth), &shupd); err != nil {
					return fmt.Errorf("getting shallow commits: %w", err)
				}
			default:
				return fmt.Errorf("unsupported depth type %T", upreq.Depth)
			}

			if err := shupd.Encode(w); err != nil {
				return fmt.Errorf("sending shallow-update: %w", err)
			}
		}

		var (
			wants = upreq.Wants
			caps  = upreq.Capabilities
		)

		// Find common commits/objects
		havesWithRef, err := revlist.ObjectsWithRef(st, wants, nil)
		if err != nil {
			return fmt.Errorf("getting objects with ref: %w", err)
		}

		// Encode objects to packfile and write to client
		multiAck := caps.Supports(capability.MultiACK)
		multiAckDetailed := caps.Supports(capability.MultiACKDetailed)

		var done bool
		var haves []plumbing.Hash
		for !done {
			var uphav packp.UploadHaves
			if err := uphav.Decode(rd); err != nil {
				return fmt.Errorf("decoding upload-haves: %w", err)
			}

			haves = append(haves, uphav.Haves...)
			done = uphav.Done

			common := map[plumbing.Hash]struct{}{}
			var ack packp.ACK
			var acks []packp.ACK
			for _, hu := range uphav.Haves {
				refs, ok := havesWithRef[hu]
				if ok {
					for _, ref := range refs {
						common[ref] = struct{}{}
					}
				}

				var status packp.ACKStatus
				if multiAckDetailed {
					status = packp.ACKCommon
					if !ok {
						status = packp.ACKReady
					}
				} else if multiAck {
					status = packp.ACKContinue
				}

				if ok || multiAck || multiAckDetailed {
					ack = packp.ACK{Hash: hu, Status: status}
					acks = append(acks, ack)
					if !multiAck && !multiAckDetailed {
						break
					}
				}
			}

			if len(haves) > 0 {
				// Encode ACKs to client when we have haves
				srvrsp := packp.ServerResponse{ACKs: acks}
				if err := srvrsp.Encode(w); err != nil {
					return fmt.Errorf("sending acks server-response: %w", err)
				}
			}

			if !done {
				if multiAck || multiAckDetailed {
					// Encode a NAK for multi-ack
					srvrsp := packp.ServerResponse{}
					if err := srvrsp.Encode(w); err != nil {
						return fmt.Errorf("sending nak server-response: %w", err)
					}
				}
			} else if !ack.Hash.IsZero() && (multiAck || multiAckDetailed) {
				// We're done, send the final ACK
				ack.Status = 0
				srvrsp := packp.ServerResponse{ACKs: []packp.ACK{ack}}
				if err := srvrsp.Encode(w); err != nil {
					return fmt.Errorf("sending final ack server-response: %w", err)
				}
			} else if ack.Hash.IsZero() {
				// We don't have multi-ack and there are no haves. Encode a NAK.
				srvrsp := packp.ServerResponse{}
				if err := srvrsp.Encode(w); err != nil {
					return fmt.Errorf("sending final nak server-response: %w", err)
				}
			}
		}

		// Done with the request, now close the reader
		// to indicate that we are done reading from it.
		if err := r.Close(); err != nil {
			return fmt.Errorf("closing reader: %w", err)
		}

		objs, err := objectsToUpload(st, wants, haves)
		if err != nil {
			w.Close() //nolint:errcheck
			return fmt.Errorf("getting objects to upload: %w", err)
		}

		var writer io.Writer = w
		if !caps.Supports(capability.NoProgress) {
			if caps.Supports(capability.Sideband64k) {
				writer = sideband.NewMuxer(sideband.Sideband64k, w)
			} else if caps.Supports(capability.Sideband) {
				writer = sideband.NewMuxer(sideband.Sideband, w)
			}
		}

		e := packfile.NewEncoder(writer, st, false)
		_, err = e.Encode(objs, 10)
		if err != nil {
			return fmt.Errorf("encoding packfile: %w", err)
		}

		if err := w.Close(); err != nil {
			return fmt.Errorf("closing writer: %w", err)
		}
	}

	return nil
}

func objectsToUpload(st storage.Storer, wants, haves []plumbing.Hash) ([]plumbing.Hash, error) {
	return revlist.Objects(st, wants, haves)
}

func getShallowCommits(st storage.Storer, heads []plumbing.Hash, depth int, upd *packp.ShallowUpdate) error {
	var i, curDepth int
	var commit *object.Commit
	depths := map[*object.Commit]int{}
	stack := []object.Object{}

	for commit != nil || i < len(heads) || len(stack) > 0 {
		if commit == nil {
			if i < len(heads) {
				obj, err := st.EncodedObject(plumbing.CommitObject, heads[i])
				i++
				if err != nil {
					continue
				}

				commit, err = object.DecodeCommit(st, obj)
				if err != nil {
					commit = nil
					continue
				}

				depths[commit] = 0
				curDepth = 0
			} else if len(stack) > 0 {
				commit = stack[len(stack)-1].(*object.Commit)
				stack = stack[:len(stack)-1]
				curDepth = depths[commit]
			}
		}

		curDepth++

		if depth != math.MaxInt && curDepth >= depth {
			upd.Shallows = append(upd.Shallows, commit.Hash)
			commit = nil
			continue
		}

		upd.Unshallows = append(upd.Unshallows, commit.Hash)

		parents := commit.Parents()
		commit = nil
		for {
			parent, err := parents.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			if depths[parent] != 0 && curDepth >= depths[parent] {
				continue
			}

			depths[parent] = curDepth

			if _, err := parents.Next(); err == nil {
				stack = append(stack, parent)
			} else {
				commit = parent
				curDepth = depths[commit]
			}
		}

	}

	return nil
}
