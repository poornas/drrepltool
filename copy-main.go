/*
 * MinIO Client (C) 2022 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/minio/cli"
	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio/pkg/console"
)

var srcFlags = []cli.Flag{
	cli.StringFlag{
		Name:  "src-endpoint",
		Usage: "S3 endpoint url",
	},
	cli.StringFlag{
		Name:  "src-access-key",
		Usage: "S3 access key",
	},
	cli.StringFlag{
		Name:  "src-secret-key",
		Usage: "S3 secret key",
	},
	cli.StringFlag{
		Name:  "src-bucket",
		Usage: "S3 bucket",
	},
	cli.IntFlag{
		Name:  "skip, s",
		Usage: "number of entries to skip from input file",
		Value: 0,
	},
	cli.BoolFlag{
		Name:  "fake",
		Usage: "perform a fake copy",
	},
	cli.StringFlag{
		Name:  "input-file",
		Usage: "file with list of entries to copy from DR",
	},
}

var copyCmd = cli.Command{
	Name:   "copy",
	Usage:  "Copy objects in data-dir/object_listing.txt and replicate to MinIO endpoint specified",
	Action: copyAction,
	Flags:  append(allFlags, srcFlags...),
	CustomHelpTemplate: `NAME:
	{{.HelpName}} - {{.Usage}}
  
  USAGE:
	{{.HelpName}}  --dir
  
  FLAGS:
	{{range .VisibleFlags}}{{.}}
	{{end}}
  
  EXAMPLES:
  1. Copy object versions in object_listing.txt of data-dir to minio bucket "dstbucket" at https://minio2 from "srcbucket" in https://minio1
	 $ drrepltool copy --data-dir "/tmp/data" --endpoint https://minio2 --access-key minio --secret-key minio123 --bucket "dstbucket" \
	  --src-endpoint https://minio1 --src-access-key minio1 --src-secret-key minio123 --src-bucket srcbucket  
  `,
}
var (
	srcClient *miniogo.Client
	tgtClient *miniogo.Client
	err       error
)

func checkCopyArgsAndInit(ctx *cli.Context) {
	debug = ctx.Bool("debug")

	srcEndpoint = ctx.String("src-endpoint")
	srcAccessKey = ctx.String("src-access-key")
	srcSecretKey = ctx.String("src-secret-key")
	srcBucket = ctx.String("src-bucket")
	tgtEndpoint = ctx.String("endpoint")
	tgtAccessKey = ctx.String("access-key")
	tgtSecretKey = ctx.String("secret-key")
	tgtBucket = ctx.String("bucket")
	dirPath = ctx.String("data-dir")
	versions = ctx.Bool("versions")
	if tgtEndpoint == "" {
		log.Fatalln("--endpoint is not provided for target")
	}

	if tgtAccessKey == "" {
		log.Fatalln("--access-key is not provided for target")
	}

	if tgtSecretKey == "" {
		log.Fatalln("--secret-key is not provided for target")
	}

	if tgtBucket == "" {
		log.Fatalln("--bucket not specified for target.")
	}

	if srcEndpoint == "" {
		log.Fatalln("--src-endpoint is not provided")
	}

	if srcAccessKey == "" {
		log.Fatalln("--src-access-key is not provided")
	}

	if srcSecretKey == "" {
		log.Fatalln("--src-secret-key is not provided")
	}

	if srcBucket == "" {
		log.Fatalln("--src-bucket not specified.")
	}
	if dirPath == "" {
		console.Fatalln(fmt.Errorf("path to working dir required, please set --data-dir flag"))
		return
	}
}

func initMinioClient(ctx *cli.Context, accessKey, secretKey, minioBucket, urlStr string) (*miniogo.Client, error) {
	target, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("unable to parse input arg %s: %v", urlStr, err)
	}

	if accessKey == "" || secretKey == "" || minioBucket == "" {
		return nil, fmt.Errorf("one or more of AccessKey:%s SecretKey: %s Bucket:%s are missing in MinIO configuration for: %s", accessKey, secretKey, minioBucket, urlStr)
	}
	options := miniogo.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: target.Scheme == "https",
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 10 * time.Second,
			TLSClientConfig: &tls.Config{
				RootCAs: mustGetSystemCertPool(),
				// Can't use SSLv3 because of POODLE and BEAST
				// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
				// Can't use TLSv1.1 because of RC4 cipher usage
				MinVersion:         tls.VersionTLS12,
				NextProtos:         []string{"http/1.1"},
				InsecureSkipVerify: ctx.Bool("insecure"),
			},
			// Set this value so that the underlying transport round-tripper
			// doesn't try to auto decode the body of objects with
			// content-encoding set to `gzip`.
			//
			// Refer:
			//    https://golang.org/src/net/http/transport.go?h=roundTrip#L1843
			DisableCompression: true,
		},
		Region:       "",
		BucketLookup: 0,
	}

	return miniogo.New(target.Host, &options)
}

func copyAction(cliCtx *cli.Context) error {
	checkCopyArgsAndInit(cliCtx)
	srcClient, err = initMinioClient(cliCtx, srcAccessKey, srcSecretKey, srcBucket, srcEndpoint)
	if err != nil {
		return fmt.Errorf("could not initialize src client %w", err)
	}
	tgtClient, err = initMinioClient(cliCtx, tgtAccessKey, tgtSecretKey, tgtBucket, tgtEndpoint)
	if err != nil {
		return fmt.Errorf("could not initialize tgt client %w", err)
	}
	ctx := context.Background()
	copyState = newcopyState(ctx)
	copyState.init(ctx)
	skip := cliCtx.Int("skip")
	dryRun = cliCtx.Bool("fake")
	start := time.Now()
	file, err := os.Open(path.Join(dirPath, objListFile))
	if err != nil {
		console.Fatalln("--input-file needs to be specified", err)
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		o := scanner.Text()
		if skip > 0 {
			skip--
			continue
		}
		slc := strings.SplitN(o, ",", 4)
		if len(slc) < 3 || len(slc) > 4 {
			logDMsg(fmt.Sprintf("error processing line :%s ", o), nil)
		}
		obj := objInfo{
			bucket:       strings.TrimSpace(slc[0]),
			object:       strings.TrimSpace(slc[1]),
			versionID:    strings.TrimSpace(slc[2]),
			deleteMarker: strings.TrimSpace(slc[3]) == "true",
		}
		copyState.queueUploadTask(obj)
		logDMsg(fmt.Sprintf("adding %s to copy queue", o), nil)
	}
	if err := scanner.Err(); err != nil {
		logDMsg(fmt.Sprintf("error processing file :%s ", objListFile), err)
		return err
	}
	copyState.finish(ctx)
	if dryRun {
		logMsg("copy dry run complete")
	} else {
		end := time.Now()
		latency := end.Sub(start).Seconds()
		count := copyState.getCount() - copyState.getFailCount()
		logMsg(fmt.Sprintf("Copied %s / %s objects with latency %d secs", humanize.Comma(int64(count)), humanize.Comma(int64(copyState.getCount())), int64(latency)))
	}
	return nil
}
