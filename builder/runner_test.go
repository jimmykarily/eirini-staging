package builder_test

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"code.cloudfoundry.org/eirini-staging/builder"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("Building", func() {

	var (
		tmpDir                    string
		buildDir                  string
		buildpacksDir             string
		outputDroplet             string
		outputMetadata            string
		outputBuildArtifactsCache string
		skipDetect                bool
		buildpackOrder            string

		runner *builder.Runner
		logOut *gbytes.Buffer

		buildpackFixtures = filepath.Join("fixtures", "buildpacks", "unix")
		appFixtures       = filepath.Join("fixtures", "apps")

		userFacingError error
	)

	cpBuildpack := func(buildpack string) {
		hash := fmt.Sprintf("%x", md5.Sum([]byte(buildpack)))
		cp(filepath.Join(buildpackFixtures, buildpack), filepath.Join(buildpacksDir, hash))
	}

	BeforeEach(func() {
		var err error

		tmpDir, err = ioutil.TempDir("", "building-tmp")
		Expect(err).NotTo(HaveOccurred())

		buildDir, err = ioutil.TempDir(tmpDir, "building-app")
		Expect(err).NotTo(HaveOccurred())

		buildpacksDir, err = ioutil.TempDir(tmpDir, "building-buildpacks")
		Expect(err).NotTo(HaveOccurred())

		outputDropletFile, err := ioutil.TempFile(tmpDir, "building-droplet")
		Expect(err).NotTo(HaveOccurred())
		outputDroplet = outputDropletFile.Name()
		Expect(outputDropletFile.Close()).To(Succeed())
		outputBuildArtifactsCacheDir, err := ioutil.TempDir(tmpDir, "building-cache-output")
		Expect(err).NotTo(HaveOccurred())
		outputBuildArtifactsCache = filepath.Join(outputBuildArtifactsCacheDir, "cache.tgz")

		outputMetadataFile, err := ioutil.TempFile(tmpDir, "building-result")
		Expect(err).NotTo(HaveOccurred())
		outputMetadata = outputMetadataFile.Name()
		Expect(outputMetadataFile.Close()).To(Succeed())

		skipDetect = false
		logOut = gbytes.NewBuffer()
		log.SetOutput(logOut)
	})

	AfterEach(func() {
		runner.CleanUp()

		Expect(os.RemoveAll(tmpDir)).To(Succeed())
	})

	JustBeforeEach(func() {
		conf := builder.Config{
			BuildDir:                  buildDir,
			BuildpacksDir:             buildpacksDir,
			OutputDropletLocation:     outputDroplet,
			OutputBuildArtifactsCache: outputBuildArtifactsCache,
			OutputMetadataLocation:    outputMetadata,
			BuildpackOrder:            strings.Split(buildpackOrder, ","),
			BuildArtifactsCache:       "/tmp/cache",
			SkipDetect:                skipDetect,
		}

		runner = builder.NewRunner(&conf)
		runner.BuildpackOut = GinkgoWriter
		runner.BuildpackErr = GinkgoWriter
		userFacingError = runner.Run()

	})

	resultJSON := func() []byte {
		resultInfo, err := ioutil.ReadFile(outputMetadata)
		Expect(err).NotTo(HaveOccurred())

		return resultInfo
	}

	resultJSONbuildpacks := func() []byte {
		result := resultJSON()
		var stagingResult builder.StagingResult
		Expect(json.Unmarshal(result, &stagingResult)).To(Succeed())
		bytes, err := json.Marshal(stagingResult.LifecycleMetadata.Buildpacks)
		Expect(err).ToNot(HaveOccurred())
		return bytes
	}

	Context("run detect", func() {
		Context("with multiple buildpacks", func() {
			BeforeEach(func() {
				buildpackOrder = "always-detects,also-always-detects"

				cpBuildpack("always-detects")
				cpBuildpack("also-always-detects")
				cp(path.Join(appFixtures, "bash-app", "app.sh"), buildDir)
			})

			Context("first buildpack detect is not executable", func() {
				BeforeEach(func() {
					hash := fmt.Sprintf("%x", md5.Sum([]byte("always-detects")))
					binDetect := filepath.Join(buildpacksDir, hash, "bin", "detect")
					Expect(os.Chmod(binDetect, 0644)).To(Succeed())
				})

				It("is successful", func() {
					Expect(userFacingError).NotTo(HaveOccurred())
				})

				It("should warn that detect is not executable", func() {
					Expect(logOut).To(gbytes.Say("WARNING: buildpack script '/bin/detect' is not executable"))
				})

				Context("first buildpack is missing the detect script", func() {
					BeforeEach(func() {
						hash := fmt.Sprintf("%x", md5.Sum([]byte("always-detects")))
						binDetect := filepath.Join(buildpacksDir, hash, "bin", "detect")
						Expect(exec.Command("rm", binDetect).Run()).To(Succeed())
					})

					It("should log the missing detect script", func() {
						Expect(logOut).To(gbytes.Say("failed to find detect script"))
					})
				})

				It("should have chosen the second buildpack detect", func() {
					data := &struct {
						LifeCycle struct {
							Key string `json:"buildpack_key"`
						} `json:"lifecycle_metadata"`
					}{}
					Eventually(func() error { return json.Unmarshal(resultJSON(), data) }).Should(Succeed())

					Expect(data.LifeCycle.Key).To(Equal("also-always-detects"))
				})

			})

			Describe("the contents of the output tgz", func() {
				var files []string

				JustBeforeEach(func() {
					Expect(userFacingError).NotTo(HaveOccurred())
					result, err := exec.Command("tar", "-tzf", outputDroplet).Output()
					Expect(err).NotTo(HaveOccurred())
					files = removeTrailingSpace(strings.Split(string(result), "\n"))
				})

				It("should contain an /app dir with the contents of the compilation", func() {
					Expect(files).To(ContainElement("./app/"))
					Expect(files).To(ContainElement("./app/app.sh"))
					Expect(files).To(ContainElement("./app/compiled"))
				})

				It("should contain an empty /tmp directory", func() {
					Expect(files).To(ContainElement("./tmp/"))
					Expect(files).NotTo(ContainElement(MatchRegexp("\\./tmp/.+")))
				})

				It("should contain an empty /logs directory", func() {
					Expect(files).To(ContainElement("./logs/"))
					Expect(files).NotTo(ContainElement(MatchRegexp("\\./logs/.+")))
				})

				Context("buildpack with supply/finalize", func() {
					BeforeEach(func() {
						buildpackOrder = "has-finalize,always-detects,also-always-detects"
						cpBuildpack("has-finalize")
					})

					It("runs supply/finalize and not compile", func() {
						Expect(files).To(ContainElement("./app/finalized"))
						Expect(files).ToNot(ContainElement("./app/compiled"))
					})

					It("places profile.d scripts in ./profile.d (not app)", func() {
						Expect(files).To(ContainElement("./profile.d/finalized.sh"))
					})
				})
			})

			Describe("the result.json, which is used to communicate back to the stager", func() {
				It("exists, and contains the detected buildpack", func() {
					Expect(resultJSON()).To(MatchJSON(`{
						"process_types":{"web":"the start command"},
						"lifecycle_type": "buildpack",
						"lifecycle_metadata":{
							"detected_buildpack": "Always Matching",
							"buildpack_key": "always-detects",
							"buildpacks": [
								{"key": "always-detects", "name": "Always Matching"}
							]
						},
						"execution_metadata": ""
				}`))
				})

				Context("when the app has a Procfile", func() {
					BeforeEach(func() {
						cp(filepath.Join(appFixtures, "with-procfile-with-web", "Procfile"), buildDir)
					})

					It("uses the Procfile processes in the execution metadata", func() {
						Expect(resultJSON()).To(MatchJSON(`{
							"process_types":{"web":"procfile-provided start-command"},
							"lifecycle_type": "buildpack",
							"lifecycle_metadata":{
								"detected_buildpack": "Always Matching",
								"buildpack_key": "always-detects",
								"buildpacks": [
									{ "key": "always-detects", "name": "Always Matching" }
								]
							},
							"execution_metadata": ""
					 }`))
					})

					It("should contain staging info about the detected buildpack", func() {
						stagingInfo, err := exec.Command("tar", "-xzf", outputDroplet, "-O", "./staging_info.yml").Output()
						Expect(err).NotTo(HaveOccurred())

						expectedYAML := `{"detected_buildpack":"Always Matching","start_command":"procfile-provided start-command"}`
						Expect(string(stagingInfo)).To(MatchJSON(expectedYAML))
					})

				})

				Context("when the app does not have a Procfile", func() {
					BeforeEach(func() {
						cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
					})

					It("uses the default_process_types specified by the buildpack", func() {
						Expect(resultJSON()).To(MatchJSON(`{
							"process_types":{"web":"the start command"},
							"lifecycle_type": "buildpack",
							"lifecycle_metadata":{
								"detected_buildpack": "Always Matching",
								"buildpack_key": "always-detects",
								"buildpacks": [
									{ "key": "always-detects", "name": "Always Matching" }
								]
							},
							"execution_metadata": ""
					 }`))
					})

					It("should contain staging info about the detected buildpack", func() {
						stagingInfo, err := exec.Command("tar", "-xzf", outputDroplet, "-O", "./staging_info.yml").Output()
						Expect(err).NotTo(HaveOccurred())

						expectedYAML := `{"detected_buildpack":"Always Matching","start_command":"the start command"}`
						Expect(string(stagingInfo)).To(MatchJSON(expectedYAML))
					})
				})
			})
		})
	})

	Context("skip detect", func() {
		BeforeEach(func() {
			skipDetect = true
		})

		Describe("the contents of the output tgz", func() {
			var files []string

			JustBeforeEach(func() {
				result, err := exec.Command("tar", "-tzf", outputDroplet).Output()
				Expect(err).NotTo(HaveOccurred())

				files = removeTrailingSpace(strings.Split(string(result), "\n"))
			})

			Describe("the result.json, which is used to communicate back to the stager", func() {
				BeforeEach(func() {
					buildpackOrder = "always-detects"
					cpBuildpack("always-detects")
					cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
				})

				It("exists, and contains the final buildpack key", func() {
					Expect(resultJSON()).To(MatchJSON(`{
							"process_types":{"web":"the start command"},
							"lifecycle_type": "buildpack",
							"lifecycle_metadata":{
								"detected_buildpack": "",
								"buildpack_key": "always-detects",
								"buildpacks": [
									{ "key": "always-detects", "name": "" }
							  ]
							},
							"execution_metadata": ""
					}`))
				})
			})

			Context("final buildpack does not contain a finalize script", func() {
				BeforeEach(func() {
					buildpackOrder = "always-detects-creates-build-artifacts,always-detects,also-always-detects"

					cpBuildpack("always-detects-creates-build-artifacts")
					cpBuildpack("always-detects")
					cpBuildpack("also-always-detects")
					cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
				})

				It("contains an /deps/xxxxx dir with the contents of the supply commands", func() {
					content, err := exec.Command("tar", "-xzOf", outputDroplet, "./deps/0/supplied").CombinedOutput()
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimRight(string(content), " \r\n")).To(Equal("always-detects-creates-buildpack-artifacts"))

					content, err = exec.Command("tar", "-xzOf", outputDroplet, "./deps/1/supplied").Output()
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimRight(string(content), " \r\n")).To(Equal("always-detects-buildpack"))

					Expect(files).ToNot(ContainElement("./deps/2/supplied"))
				})

				It("contains an /app dir with the contents of the compilation", func() {
					Expect(files).To(ContainElement("./app/"))
					Expect(files).To(ContainElement("./app/app.sh"))
					Expect(files).To(ContainElement("./app/compiled"))

					content, err := exec.Command("tar", "-xzOf", outputDroplet, "./app/compiled").Output()
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimRight(string(content), " \r\n")).To(Equal("also-always-detects-buildpack"))
				})

				It("the /deps dir is not passed to the final compile command", func() {
					Expect(files).ToNot(ContainElement("./deps/compiled"))
				})
			})

			Context("final buildpack contains finalize + supply scripts", func() {
				BeforeEach(func() {
					buildpackOrder = "always-detects-creates-build-artifacts,always-detects,has-finalize"

					cpBuildpack("always-detects-creates-build-artifacts")
					cpBuildpack("always-detects")
					cpBuildpack("has-finalize")
					cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
				})

				It("contains an /deps/xxxxx dir with the contents of the supply commands", func() {
					content, err := exec.Command("tar", "-xzOf", outputDroplet, "./deps/0/supplied").Output()
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimRight(string(content), " \r\n")).To(Equal("always-detects-creates-buildpack-artifacts"))

					content, err = exec.Command("tar", "-xzOf", outputDroplet, "./deps/1/supplied").Output()
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimRight(string(content), " \r\n")).To(Equal("always-detects-buildpack"))

					content, err = exec.Command("tar", "-xzOf", outputDroplet, "./deps/2/supplied").Output()
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimRight(string(content), " \r\n")).To(Equal("has-finalize-buildpack"))
				})

				It("contains an /app dir with the contents of the compilation", func() {
					Expect(files).To(ContainElement("./app/"))
					Expect(files).To(ContainElement("./app/app.sh"))
					Expect(files).To(ContainElement("./app/finalized"))

					content, err := exec.Command("tar", "-xzOf", outputDroplet, "./app/finalized").Output()
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimRight(string(content), " \r\n")).To(Equal("has-finalize-buildpack"))
				})

				It("writes metadata on all buildpacks", func() {
					Expect(resultJSONbuildpacks()).To(MatchJSON(`[
							  { "key": "always-detects-creates-build-artifacts", "name": "Creates Buildpack Artifacts", "version": "9.1.3" },
								{ "key": "always-detects", "name": "" },
								{ "key": "has-finalize", "name": "Finalize" }
							]`))
				})
			})

			Context("final buildpack only contains finalize ", func() {
				BeforeEach(func() {
					buildpackOrder = "always-detects-creates-build-artifacts,always-detects,has-finalize-no-supply"

					cpBuildpack("always-detects-creates-build-artifacts")
					cpBuildpack("always-detects")
					cpBuildpack("has-finalize-no-supply")
					cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
				})

				It("contains an /deps/xxxxx dir with the contents of the supply commands", func() {
					content, err := exec.Command("tar", "-xzOf", outputDroplet, "./deps/0/supplied").Output()
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimRight(string(content), " \r\n")).To(Equal("always-detects-creates-buildpack-artifacts"))

					content, err = exec.Command("tar", "-xzOf", outputDroplet, "./deps/1/supplied").Output()
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimRight(string(content), " \r\n")).To(Equal("always-detects-buildpack"))

					Expect(files).ToNot(ContainElement("./deps/2/supplied"))
				})

				It("contains an /app dir with the contents of the compilation", func() {
					Expect(files).To(ContainElement("./app/"))
					Expect(files).To(ContainElement("./app/app.sh"))
					Expect(files).To(ContainElement("./app/finalized"))

					content, err := exec.Command("tar", "-xzOf", outputDroplet, "./app/finalized").Output()
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimRight(string(content), " \r\n")).To(Equal("has-finalize-no-supply-buildpack"))
				})
			})

			Context("buildpack that fails detect", func() {
				BeforeEach(func() {
					buildpackOrder = "always-fails-detect"

					cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
					cpBuildpack("always-fails-detect")
				})

				It("should run successfully", func() {
					Expect(userFacingError).NotTo(HaveOccurred())
					Expect(files).To(ContainElement("./app/compiled"))
				})
			})
		})
	})

	Context("with a buildpack that has no commands", func() {
		BeforeEach(func() {
			buildpackOrder = "release-without-command"
			cpBuildpack("release-without-command")
		})

		Context("when release script is missing", func() {
			BeforeEach(func() {
				hash := fmt.Sprintf("%x", md5.Sum([]byte("release-without-command")))
				binDetect := filepath.Join(buildpacksDir, hash, "bin", "release")
				Expect(os.Chmod(binDetect, 0644)).To(Succeed())
			})

			It("should log the error", func() {
				Expect(userFacingError).To(MatchError(ContainSubstring("no release script")))
			})
		})

		Context("when there is a release script", func() {
			Context("when the app has a Procfile", func() {
				Context("with web defined", func() {
					BeforeEach(func() {
						cp(filepath.Join(appFixtures, "with-procfile-with-web", "Procfile"), buildDir)
					})

					It("is successful", func() {
						Expect(userFacingError).NotTo(HaveOccurred())
					})

					It("uses the Procfile for execution_metadata", func() {
						Expect(resultJSON()).To(MatchJSON(`{
								"process_types":{"web":"procfile-provided start-command"},
								"lifecycle_type": "buildpack",
								"lifecycle_metadata":{
									"detected_buildpack": "Release Without Command",
									"buildpack_key": "release-without-command",
									"buildpacks": [
										{ "key": "release-without-command", "name": "Release Without Command" }
									]
								},
								"execution_metadata": ""
							}`))
					})
				})

				Context("without web", func() {
					BeforeEach(func() {
						cp(filepath.Join(appFixtures, "with-procfile", "Procfile"), buildDir)
					})

					It("is successful", func() {
						Expect(userFacingError).NotTo(HaveOccurred())
					})

					It("displays an error and returns the Procfile data without web", func() {
						Expect(logOut).To(gbytes.Say("No start command specified by buildpack or via Procfile."))
						Expect(logOut).To(gbytes.Say("App will not start unless a command is provided at runtime."))

						Expect(resultJSON()).To(MatchJSON(`{
								"process_types":{"spider":"bogus command"},
								"lifecycle_type": "buildpack",
								"lifecycle_metadata": {
									"detected_buildpack": "Release Without Command",
									"buildpack_key": "release-without-command",
									"buildpacks": [
										{ "key": "release-without-command", "name": "Release Without Command" }
									]
								},
								"execution_metadata": ""
							}`))
					})
				})
			})

			Context("and the app has no Procfile", func() {
				BeforeEach(func() {
					cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
				})

				It("outputs log messages", func() {
					Expect(logOut).To(gbytes.Say("No start command specified by buildpack or via Procfile."))
					Expect(logOut).To(gbytes.Say("App will not start unless a command is provided at runtime."))
				})

				It("is successful", func() {
					Expect(userFacingError).NotTo(HaveOccurred())
				})
			})
		})
	})

	Context("with a buildpack that determines a start web-command", func() {

		BeforeEach(func() {
			buildpackOrder = "always-detects"
			cpBuildpack("always-detects")
		})

		Context("when the app has a Procfile", func() {
			Context("with web defined", func() {
				BeforeEach(func() {
					cp(filepath.Join(appFixtures, "with-procfile-with-web", "Procfile"), buildDir)
				})

				It("is successful", func() {
					Expect(userFacingError).NotTo(HaveOccurred())
				})

				It("merges the Procfile and the buildpack for execution_metadata", func() {
					Expect(resultJSON()).To(MatchJSON(`{
						"process_types":{"web":"procfile-provided start-command"},
						"lifecycle_type": "buildpack",
						"lifecycle_metadata":{
							"detected_buildpack": "Always Matching",
							"buildpack_key": "always-detects",
							"buildpacks": [
							  { "key": "always-detects", "name": "Always Matching" }
						  ]
						},
						"execution_metadata": ""
					}`))
				})
			})

			Context("without web", func() {
				BeforeEach(func() {
					cp(filepath.Join(appFixtures, "with-procfile", "Procfile"), buildDir)
				})

				It("is successful", func() {
					Expect(userFacingError).NotTo(HaveOccurred())
				})

				It("merges the Procfile but uses the buildpack for execution_metadata", func() {

					Expect(resultJSON()).To(MatchJSON(`{
						"process_types":{"spider":"bogus command", "web":"the start command"},
						"lifecycle_type": "buildpack",
						"lifecycle_metadata": {
							"detected_buildpack": "Always Matching",
							"buildpack_key": "always-detects",
							"buildpacks": [
							  { "key": "always-detects", "name": "Always Matching" }
						  ]
						},
						"execution_metadata": ""
					}`))
				})
			})
		})

		Context("and the app has no Procfile", func() {
			BeforeEach(func() {
				cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
			})

			It("merges the Procfile and the buildpack for execution_metadata", func() {
				Expect(resultJSON()).To(MatchJSON(`{
						"process_types":{"web":"the start command"},
						"lifecycle_type": "buildpack",
						"lifecycle_metadata":{
							"detected_buildpack": "Always Matching",
							"buildpack_key": "always-detects",
							"buildpacks": [
							  { "key": "always-detects", "name": "Always Matching" }
						  ]
						},
						"execution_metadata": ""
					}`))
			})

			It("is successful", func() {
				Expect(userFacingError).NotTo(HaveOccurred())
			})
		})
	})

	Context("with a buildpack that determines a start non-web-command", func() {

		BeforeEach(func() {
			buildpackOrder = "always-detects-non-web"
			cpBuildpack("always-detects-non-web")
		})

		Context("when the app has a Procfile", func() {
			Context("with web defined", func() {
				BeforeEach(func() {
					cp(filepath.Join(appFixtures, "with-procfile-with-web", "Procfile"), buildDir)
				})

				It("is successful", func() {
					Expect(userFacingError).NotTo(HaveOccurred())
				})

				It("merges the Procfile for execution_metadata", func() {
					Expect(resultJSON()).To(MatchJSON(`{
						"process_types":{"web":"procfile-provided start-command", "nonweb":"start nonweb buildpack"},
						"lifecycle_type": "buildpack",
						"lifecycle_metadata":{
							"detected_buildpack": "Always Detects Non-Web",
							"buildpack_key": "always-detects-non-web",
						  "buildpacks": [
	                { "key": "always-detects-non-web", "name": "Always Detects Non-Web" }
	            ]
						},
						"execution_metadata": ""
					}`))
				})
			})

			Context("without web", func() {
				BeforeEach(func() {
					cp(filepath.Join(appFixtures, "with-procfile", "Procfile"), buildDir)
				})

				It("is successful", func() {
					Expect(userFacingError).NotTo(HaveOccurred())
				})

				It("displays an error and returns the Procfile data without web", func() {
					Expect(logOut).To(gbytes.Say("No start command specified by buildpack or via Procfile."))
					Expect(logOut).To(gbytes.Say("App will not start unless a command is provided at runtime."))

					Expect(resultJSON()).To(MatchJSON(`{
						"process_types":{"spider":"bogus command", "nonweb":"start nonweb buildpack"},
						"lifecycle_type": "buildpack",
						"lifecycle_metadata": {
							"detected_buildpack": "Always Detects Non-Web",
							"buildpack_key": "always-detects-non-web",
						  "buildpacks": [
	                { "key": "always-detects-non-web", "name": "Always Detects Non-Web" }
	            ]
						},
						"execution_metadata": ""
					}`))
				})
			})
		})

		Context("and the app has no Procfile", func() {
			BeforeEach(func() {
				cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
			})

			It("is successful", func() {
				Expect(userFacingError).NotTo(HaveOccurred())
			})

			It("outputs log messages", func() {
				Expect(logOut).To(gbytes.Say("No start command specified by buildpack or via Procfile."))
				Expect(logOut).To(gbytes.Say("App will not start unless a command is provided at runtime."))
				Expect(resultJSON()).To(MatchJSON(`{
						"process_types":{"nonweb":"start nonweb buildpack"},
						"lifecycle_type": "buildpack",
						"lifecycle_metadata": {
							"detected_buildpack": "Always Detects Non-Web",
							"buildpack_key": "always-detects-non-web",
						  "buildpacks": [
	                { "key": "always-detects-non-web", "name": "Always Detects Non-Web" }
	            ]
						},
						"execution_metadata": ""
					}`))
			})
		})
	})

	Context("with an app with an invalid Procfile", func() {
		BeforeEach(func() {
			buildpackOrder = "always-detects,also-always-detects"

			cpBuildpack("always-detects")
			cpBuildpack("also-always-detects")

			cp(filepath.Join(appFixtures, "bogus-procfile", "Procfile"), buildDir)
		})

		It("fails", func() {
			Expect(userFacingError).To(MatchError(ContainSubstring("Failed to read command from Procfile")))
		})
	})

	Context("when no buildpacks match", func() {
		BeforeEach(func() {
			buildpackOrder = "always-fails-detect"

			cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
			cpBuildpack("always-fails-detect")
		})

		It("should exit with an error", func() {
			Expect(userFacingError).To(MatchError(ContainSubstring("None of the buildpacks detected a compatible application")))
			Expect(userFacingError.(builder.DescriptiveError).ExitCode).To(Equal(222))
		})
	})

	Context("when the buildpack fails in compile", func() {
		BeforeEach(func() {
			buildpackOrder = "fails-to-compile"

			cpBuildpack("fails-to-compile")
			cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
		})

		It("should exit with an error", func() {
			Expect(userFacingError).To(MatchError(ContainSubstring("failed to compile droplet")))
			Expect(userFacingError.(builder.DescriptiveError).ExitCode).To(Equal(223))
		})

		It("should log the error", func() {
			Expect(logOut).To(gbytes.Say("compile script failed"))
		})
	})

	Context("when a buildpack fails a supply script", func() {
		BeforeEach(func() {
			buildpackOrder = "fails-to-supply,always-detects"
			skipDetect = true

			cpBuildpack("fails-to-supply")
			cpBuildpack("always-detects")
			cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
		})

		It("should exit with an error", func() {
			Expect(userFacingError).To(MatchError(ContainSubstring("Failed to run all supply scripts")))
			Expect(userFacingError.(builder.DescriptiveError).ExitCode).To(Equal(225))
		})

		It("should log the error", func() {
			Expect(logOut).To(gbytes.Say("supply script failed"))
		})
	})

	Context("when a buildpack that isn't last doesn't have a supply script", func() {
		BeforeEach(func() {
			buildpackOrder = "has-finalize-no-supply,has-finalize"
			skipDetect = true

			cpBuildpack("has-finalize-no-supply")
			cpBuildpack("has-finalize")
			cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
		})

		It("should exit with a clear error", func() {
			Expect(userFacingError).To(MatchError(ContainSubstring("Error: one of the buildpacks chosen to supply dependencies does not support multi-buildpack apps")))
			Expect(userFacingError.(builder.DescriptiveError).ExitCode).To(Equal(225))
		})

		It("should log the error", func() {
			Expect(logOut).To(gbytes.Say("supply script missing"))
		})
	})

	Context("when a the final buildpack has compile but not finalize", func() {
		Context("single buildpack", func() {
			BeforeEach(func() {
				buildpackOrder = "always-detects"
				skipDetect = true

				cpBuildpack("always-detects")
				cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
			})

			It("is successful", func() {
				Expect(userFacingError).NotTo(HaveOccurred())
			})

			It("should not display a warning about multi-buildpack compatibility", func() {
				Expect(logOut).NotTo(gbytes.Say("the last buildpack is not compatible with multi-buildpack apps"))
			})
		})

		Context("multi-buildpack", func() {
			BeforeEach(func() {
				buildpackOrder = "has-finalize,always-detects"
				skipDetect = true

				cpBuildpack("has-finalize")
				cpBuildpack("always-detects")
				cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
			})

			It("is successful", func() {
				Expect(userFacingError).NotTo(HaveOccurred())
			})

			It("should display a warning about multi-buildpack compatibility", func() {
				Expect(logOut).To(gbytes.Say("Warning: the last buildpack is not compatible with multi-buildpack apps and cannot make use of any dependencies supplied by the buildpacks specified before it"))
			})
		})
	})

	Context("when the buildpack release generates invalid yaml", func() {
		BeforeEach(func() {
			buildpackOrder = "release-generates-bad-yaml"

			cpBuildpack("release-generates-bad-yaml")
			cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
		})

		It("should exit with an error", func() {
			Expect(userFacingError).To(MatchError(ContainSubstring("buildpack's release output invalid")))
			Expect(userFacingError.(builder.DescriptiveError).ExitCode).To(Equal(224))
		})
	})

	Context("when the buildpack fails to release", func() {
		BeforeEach(func() {
			buildpackOrder = "fails-to-release"

			cpBuildpack("fails-to-release")
			cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
		})

		It("should exit with an error", func() {
			Expect(userFacingError).To(MatchError(ContainSubstring("Failed to build droplet release")))
			Expect(userFacingError.(builder.DescriptiveError).ExitCode).To(Equal(224))
		})
	})

	Context("with a nested buildpack", func() {
		BeforeEach(func() {
			nestedBuildpack := "nested-buildpack"
			buildpackOrder = nestedBuildpack

			nestedBuildpackHash := "70d137ae4ee01fbe39058ccdebf48460"

			nestedBuildpackDir := filepath.Join(buildpacksDir, nestedBuildpackHash)
			Expect(os.MkdirAll(nestedBuildpackDir, 0777)).To(Succeed())

			cp(filepath.Join(buildpackFixtures, "always-detects"), nestedBuildpackDir)
			cp(filepath.Join(appFixtures, "bash-app", "app.sh"), buildDir)
		})

		It("should detect the nested buildpack", func() {
			Expect(userFacingError).NotTo(HaveOccurred())
		})
	})
})

func removeTrailingSpace(dirty []string) []string {
	clean := []string{}
	for _, s := range dirty {
		clean = append(clean, strings.TrimRight(s, "\r\n"))
	}

	return clean
}

func cp(src string, dst string) {
	session, err := gexec.Start(
		exec.Command("cp", "-a", src, dst),
		GinkgoWriter,
		GinkgoWriter,
	)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session).Should(gexec.Exit(0))
}
