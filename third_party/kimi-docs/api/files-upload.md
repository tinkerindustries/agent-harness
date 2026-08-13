---
title: Upload File
source: https://platform.kimi.ai/docs/api/files-upload
fetched: 2026-08-13
---

# Upload File

> Uploads a file for extraction, image understanding, or video understanding.

<Note>
  Each user can upload a maximum of 1,000 files. Each file must not exceed 100 MB, and the total size of all uploaded files must not exceed 10 GB. The file parsing service is currently free, but rate limiting may be applied during peak traffic periods.
</Note>

<Accordion title="Supported Formats">
  Supported formats include `.pdf`, `.txt`, `.csv`, `.doc`, `.docx`, `.xls`, `.xlsx`, `.ppt`, `.pptx`, `.md`, `.jpeg`, `.png`, `.bmp`, `.gif`, `.webp`, `.ico`, `.xbm`, `.dib`, `.pjp`, `.tif`, `.pjpeg`, `.avif`, `.dot`, `.apng`, `.epub`, `.tiff`, `.jfif`, `.html`, `.json`, `.mobi`, `.log`, `.go`, `.h`, `.c`, `.cpp`, `.cxx`, `.cc`, `.cs`, `.java`, `.js`, `.css`, `.jsp`, `.php`, `.py`, `.py3`, `.asp`, `.yaml`, `.yml`, `.ini`, `.conf`, `.ts`, `.tsx`, and more.
</Accordion>

<Accordion title="File Content Extraction Example">
  When uploading a file, use `purpose="file-extract"` if you want the model to use the extracted file contents as context.

  <Tabs>
    <Tab title="python">
      ```python showLineNumbers expandable theme={null}
      from pathlib import Path
      import os

      from openai import OpenAI

      client = OpenAI(
          api_key=os.environ["MOONSHOT_API_KEY"],
          base_url = "https://api.moonshot.ai/v1",
      )

      # xlnet.pdf is an example file; we support pdf, doc, and image formats.
      file_object = client.files.create(file=Path("xlnet.pdf"), purpose="file-extract")

      # Note: retrieve_content is deprecated in the latest version.
      # If you are using the latest SDK, use files.content instead.
      file_content = client.files.content(file_id=file_object.id).text

      messages = [
          {
              "role": "system",
              "content": "You are Kimi, an AI assistant provided by Moonshot AI. You are particularly skilled in Chinese and English conversations. You provide users with safe, helpful, and accurate answers. You will refuse to answer any questions involving terrorism, racism, pornography, or violence. Moonshot AI is a proper noun and should not be translated into other languages.",
          },
          {
              "role": "system",
              "content": file_content,
          },
          {"role": "user", "content": "Please give a brief introduction of what xlnet.pdf is about"},
      ]

      completion = client.chat.completions.create(
        model="kimi-k2-turbo-preview",
        messages=messages,
        temperature=0.6,
      )

      print(completion.choices[0].message)
      ```
    </Tab>

    <Tab title="curl">
      ```bash showLineNumbers theme={null}
      # xlnet.pdf is a sample file

      curl https://api.moonshot.ai/v1/files \
        -H "Authorization: Bearer $MOONSHOT_API_KEY" \
        -F purpose="file-extract" \
        -F file="@xlnet.pdf"
      ```
    </Tab>

    <Tab title="node.js">
      ```js showLineNumbers expandable theme={null}
      const OpenAI = require("openai");
      const fs = require("fs");

      const client = new OpenAI({
          apiKey: process.env.MOONSHOT_API_KEY,
          baseURL: "https://api.moonshot.ai/v1",
      });

      async function main() {
          let file_object = await client.files.create({
              file: fs.createReadStream("xlnet.pdf"),
              purpose: "file-extract"
          });

          // retrieve_content is deprecated in the latest version.
          let file_content = await (await client.files.content(file_object.id)).text();

          let messages = [
              {
                  "role": "system",
                  "content": "You are Kimi, an AI assistant provided by Moonshot AI. You are more proficient in Chinese and English conversations. You provide users with safe, helpful, and accurate answers. You will refuse to answer any questions related to terrorism, racism, pornography, or violence. Moonshot AI is a proper noun and should not be translated into other languages.",
              },
              {
                  "role": "system",
                  "content": file_content,
              },
              {"role": "user", "content": "Please give a brief introduction of what xlnet.pdf is about"},
          ];

          const completion = await client.chat.completions.create({
              model: "kimi-k2-turbo-preview",
              messages: messages,
              temperature: 0.6
          });
          console.log(completion.choices[0].message.content);
      }

      main();
      ```
    </Tab>
  </Tabs>

  Replace `$MOONSHOT_API_KEY` with your own API key, or set it as an environment variable before making the call.
</Accordion>

<Accordion title="Multi-file Chat Example">
  If you want to upload multiple files and have a conversation with Kimi based on these files, you can use the following pattern:

  ```python expandable theme={null}
  from typing import *

  import os
  import json
  from pathlib import Path

  from openai import OpenAI

  client = OpenAI(
      base_url="https://api.moonshot.ai/v1",
      api_key=os.environ["MOONSHOT_DEMO_API_KEY"],
  )


  def upload_files(files: List[str]) -> List[Dict[str, Any]]:
      """
      upload_files uploads all provided files (paths) via the file upload API '/v1/files',
      retrieves the uploaded file content, and generates file messages. Each file becomes
      an independent message with role set to system. The Kimi model will correctly
      recognize the file content in these system messages.

      :param files: A list of file paths to upload. Paths can be absolute or relative,
          passed as strings.
      :return: A list of messages containing file content. Add these messages to the Context,
          i.e., the messages parameter when calling the `/v1/chat/completions` API.
      """
      messages = []

      for file in files:
          file_object = client.files.create(file=Path(file), purpose="file-extract")
          file_content = client.files.content(file_id=file_object.id).text
          messages.append({
              "role": "system",
              "content": file_content,
          })

      return messages


  def main():
      file_messages = upload_files(files=["upload_files.py"])

      messages = [
          *file_messages,
          {
              "role": "system",
              "content": "You are Kimi, an AI assistant provided by Moonshot AI. You are more proficient in Chinese and English conversations. You provide users with safe, helpful, and accurate answers. You will refuse to answer any questions related to terrorism, racism, pornography, or violence. Moonshot AI is a proper noun and should not be translated into other languages.",
          },
          {
              "role": "user",
              "content": "Summarize the content of these files.",
          },
      ]

      print(json.dumps(messages, indent=2, ensure_ascii=False))

      completion = client.chat.completions.create(
          model="kimi-k2-turbo-preview",
          messages=messages,
      )

      print(completion.choices[0].message.content)


  if __name__ == '__main__':
      main()
  ```
</Accordion>

<Accordion title="Image or Video Understanding">
  When uploading image or video assets for native model understanding, use `purpose="image"` or `purpose="video"`.

  Please refer to [Using Vision Models](../guide/use-kimi-vision-model.md) for end-to-end examples.
</Accordion>


## OpenAPI

````yaml POST /v1/files
openapi: 3.1.0
info:
  title: Moonshot AI API
  version: 1.0.0
  description: API for Moonshot AI / Kimi large language model services
servers:
  - url: https://api.moonshot.ai
    description: Production
security: []
paths:
  /v1/files:
    post:
      tags:
        - Files
      summary: Upload File
      description: >-
        Uploads a file for extraction, image understanding, or video
        understanding.
      requestBody:
        required: true
        content:
          multipart/form-data:
            schema:
              type: object
              properties:
                file:
                  type: string
                  format: binary
                  description: The file to upload
                purpose:
                  type: string
                  enum:
                    - file-extract
                    - image
                    - video
                    - batch
                  description: >-
                    Specifies how the uploaded file will be processed.
                    file-extract: extract file contents; image: upload images
                    for vision understanding; video: upload videos for video
                    understanding; batch: upload JSONL files for batch
                    processing
              required:
                - file
                - purpose
      responses:
        '200':
          description: Uploaded file metadata
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/FileObject'
        '400':
          description: Bad request - Invalid upload parameters
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
        '401':
          description: Unauthorized - Invalid or missing API key
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
        '500':
          description: Server error
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
      security:
        - bearerAuth: []
components:
  schemas:
    FileObject:
      type: object
      properties:
        id:
          type: string
          description: Unique file identifier
        object:
          type: string
          description: Object type
          example: file
        bytes:
          type: integer
          description: File size in bytes
        created_at:
          type: integer
          description: Unix timestamp when the file was created
        filename:
          type: string
          description: Original file name
        purpose:
          type: string
          description: >-
            Purpose used when uploading the file. file-extract: extract file
            contents; image: upload images for vision understanding; video:
            upload videos for video understanding; batch: upload JSONL files for
            batch processing
          enum:
            - file-extract
            - image
            - video
            - batch
        status:
          type: string
          description: Processing status of the file
          example: ready
        status_details:
          type: string
          description: Additional status details when processing fails or returns warnings
      required:
        - id
        - object
        - bytes
        - created_at
        - filename
        - purpose
        - status
    ErrorResponse:
      type: object
      properties:
        error:
          type: object
          properties:
            message:
              type: string
              description: Error message describing what went wrong
            type:
              type: string
              description: Error type
            code:
              type: string
              description: Error code
          required:
            - message
      required:
        - error
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
      description: >-
        The Authorization header expects a Bearer token. Use an MOONSHOT_API_KEY
        as the token. This is a server-side secret key. Generate one on the [API
        keys page](https://platform.kimi.ai/console/api-keys) in your dashboard.

````
