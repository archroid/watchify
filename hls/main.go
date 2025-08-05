package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	log "github.com/sirupsen/logrus"

	"github.com/pkg/errors"
	"github.com/yutopp/go-flv"
	flvtag "github.com/yutopp/go-flv/tag"
	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

func main() {
	sigs := make(chan os.Signal, 1)                      // Buffered channel to hold one signal
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM) // Listen for Ctrl+C (SIGINT) and graceful termination (SIGTERM)

	go func() {
		sig := <-sigs // Block until a signal is received
		fmt.Println("Received signal:", sig)
		// Perform cleanup or other actions here
		fmt.Println("Performing graceful shutdown...")
		os.Remove("public/ali/index.m3u8")
		os.Remove("public/ali/*.ts")
		os.Exit(0) // Exit the program after handling
	}()

	http.Handle("/", http.FileServer(http.Dir("public/ali")))
	go http.ListenAndServe(":10500", nil)

	tcpAddr, err := net.ResolveTCPAddr("tcp", ":1935")
	if err != nil {
		log.Panicf("Failed: %+v", err)
	}

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		log.Panicf("Failed: %+v", err)
	}

	srv := rtmp.NewServer(&rtmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *rtmp.ConnConfig) {
			l := log.StandardLogger()
			h := &Handler{}
			return conn, &rtmp.ConnConfig{
				Handler: h,
				Logger:  l,
			}
		},
	})

	if err := srv.Serve(listener); err != nil {
		log.Panicf("Failed: %+v", err)
	}

}

type Handler struct {
	rtmp.DefaultHandler
	ffmpegCmd *exec.Cmd
	ffmpegIn  io.WriteCloser
	flvEnc    *flv.Encoder
}

func (h *Handler) OnPublish(_ *rtmp.StreamContext, timestamp uint32, cmd *rtmpmsg.NetStreamPublish) error {
	log.Printf("OnPublish: %#v", cmd)

	if cmd.PublishingName == "" {
		return errors.New("PublishingName is empty")
	}

	outputDir := filepath.Join("public", filepath.Clean(cmd.PublishingName))
	err := os.MkdirAll(outputDir, 0755)
	if err != nil {
		return errors.Wrap(err, "Failed to create output dir")
	}

	m3u8Path := filepath.Join(outputDir, "index.m3u8")

	// Start ffmpeg process
	ffmpegCmd := exec.Command("ffmpeg",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-i", "pipe:0",
		"-c:v", "copy",
		"-c:a", "aac",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "5",
		"-hls_flags", "split_by_time+delete_segments+program_date_time",
		m3u8Path,
	)

	ffmpegStdin, err := ffmpegCmd.StdinPipe()
	if err != nil {
		return errors.Wrap(err, "Failed to get ffmpeg stdin")
	}

	ffmpegCmd.Stdout = os.Stdout
	ffmpegCmd.Stderr = os.Stderr

	if err := ffmpegCmd.Start(); err != nil {
		return errors.Wrap(err, "Failed to start ffmpeg")
	}

	h.ffmpegCmd = ffmpegCmd
	h.ffmpegIn = ffmpegStdin

	enc, err := flv.NewEncoder(ffmpegStdin, flv.FlagsAudio|flv.FlagsVideo)
	if err != nil {
		ffmpegStdin.Close()
		ffmpegCmd.Process.Kill()
		return errors.Wrap(err, "Failed to create FLV encoder")
	}
	h.flvEnc = enc

	return nil
}

func (h *Handler) OnSetDataFrame(timestamp uint32, data *rtmpmsg.NetStreamSetDataFrame) error {
	r := bytes.NewReader(data.Payload)

	var script flvtag.ScriptData
	if err := flvtag.DecodeScriptData(r, &script); err != nil {
		log.Printf("Failed to decode script data: %+v", err)
		return nil
	}

	if err := h.flvEnc.Encode(&flvtag.FlvTag{
		TagType:   flvtag.TagTypeScriptData,
		Timestamp: timestamp,
		Data:      &script,
	}); err != nil {
		log.Printf("Failed to write script data: %+v", err)
	}
	return nil
}

func (h *Handler) OnAudio(timestamp uint32, payload io.Reader) error {
	var audio flvtag.AudioData
	if err := flvtag.DecodeAudioData(payload, &audio); err != nil {
		return err
	}

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, audio.Data); err != nil {
		return err
	}
	audio.Data = buf

	if err := h.flvEnc.Encode(&flvtag.FlvTag{
		TagType:   flvtag.TagTypeAudio,
		Timestamp: timestamp,
		Data:      &audio,
	}); err != nil {
		log.Printf("Failed to write audio: %+v", err)
	}
	return nil
}

func (h *Handler) OnVideo(timestamp uint32, payload io.Reader) error {
	var video flvtag.VideoData
	if err := flvtag.DecodeVideoData(payload, &video); err != nil {
		return err
	}

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, video.Data); err != nil {
		return err
	}
	video.Data = buf

	if err := h.flvEnc.Encode(&flvtag.FlvTag{
		TagType:   flvtag.TagTypeVideo,
		Timestamp: timestamp,
		Data:      &video,
	}); err != nil {
		log.Printf("Failed to write video: %+v", err)
	}
	return nil
}

func (h *Handler) OnClose() {
	log.Println("Client disconnected")

	if h.flvEnc != nil {
		_ = h.ffmpegIn.Close()
	}

	if h.ffmpegCmd != nil && h.ffmpegCmd.Process != nil {
		_ = h.ffmpegCmd.Process.Kill()
	}

}
