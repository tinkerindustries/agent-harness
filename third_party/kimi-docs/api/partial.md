---
title: Partial Mode
source: https://platform.kimi.ai/docs/api/partial
fetched: 2026-08-13
---

# Partial Mode

When using large language models, sometimes we want to guide the model's output by prefilling part of the response. In the Kimi large language model, we provide Partial Mode to achieve this. It helps control the output format, guide the content, and maintain better consistency in role-playing scenarios. Simply add `"partial": True` to the last message entry with `role` of `assistant` to enable Partial Mode.

```json theme={null}
{"role": "assistant", "content": "prefilled content", "partial": true}
```

<Note>
  **Note!**

  Do not mix Partial Mode with response\_format=json\_object, or you may get unexpected model responses.
</Note>

## Examples

### JSON Mode

Here is an example of using Partial Mode to achieve JSON Mode.

<CodeGroup>
  ```python Python expandable theme={null}
  import os

  from openai import OpenAI

  client = OpenAI(
      api_key=os.environ["MOONSHOT_API_KEY"],
      base_url="https://api.moonshot.ai/v1",
  )

  completion = client.chat.completions.create(
      model="kimi-k3",
      messages=[
          {
              "role": "system",
              "content": "Extract the name, size, price, and colors from the product description and output them in a JSON object.",
          },
          {
              "role": "user",
              "content": "The DaMi SmartHome Mini is a compact smart home assistant available in black and silver. It costs 998 yuan and measures 256 x 128 x 128mm. It allows you to control lights, thermostats, and other connected devices via voice or app, no matter where you place it in your home.",
          },
          {
              "role": "assistant",
              "content": "{",
              "partial": True
          },
      ]
  )

  print('{'+completion.choices[0].message.content)
  ```

  ```bash cURL theme={null}
  curl https://api.moonshot.ai/v1/chat/completions \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer $MOONSHOT_API_KEY" \
      -d '{
          "model": "kimi-k3",
          "messages": [
              {"role": "system", "content": "Extract the name, size, price, and colors from the product description and output them in a JSON object."},
              {"role": "user", "content": "The DaMi SmartHome Mini is a compact smart home assistant available in black and silver. It costs 998 yuan and measures 256 x 128 x 128mm. It allows you to control lights, thermostats, and other connected devices via voice or app, no matter where you place it in your home."},
              {"role": "assistant", "content": "{", "partial": true}
          ]
     }'
  ```

  ```javascript Node.js expandable theme={null}
  const OpenAI = require("openai");

  const client = new OpenAI({
      apiKey: process.env.MOONSHOT_API_KEY,
      baseURL: "https://api.moonshot.ai/v1",
  });

  async function main() {
      const completion = await client.chat.completions.create({
          model: "kimi-k3",
          messages: [
              {role: "system", content: "Extract the name, size, price, and colors from the product description and output them in a JSON object."},
              {role: "user", content: "The DaMi SmartHome Mini is a compact smart home assistant available in black and silver. It costs 998 yuan and measures 256 x 128 x 128mm. It allows you to control lights, thermostats, and other connected devices via voice or app, no matter where you place it in your home."},
              {role: "assistant", content: "{", partial: true}
          ]
      });
      console.log("{"+completion.choices[0].message.content);
  }

  main();
  ```
</CodeGroup>

Running the above code returns:

```json theme={null}
{"name": "SmartHome Mini", "size": "256 x 128 x 128mm", "price": "998 yuan", "colors": ["black", "silver"]}
```

Note that the API response does not include the leading\_text. To get the full response, you need to manually concatenate it.

### Role-Play

Based on the same principle, we can also enhance role-playing consistency by adding character information in Partial Mode. Let's use Dr. Kal'tsit from the mobile game Arknights as an example.
Note that we can also use the `"name":"Kal'tsit"` field on top of Partial Mode to better maintain the character's consistency. Here, the name field can be considered as part of the output prefix.

<CodeGroup>
  ```python Python theme={null}
  import os

  from openai import OpenAI

  client = OpenAI(
      api_key=os.environ["MOONSHOT_API_KEY"],
      base_url="https://api.moonshot.ai/v1",
  )

  completion = client.chat.completions.create(
      model="kimi-k3",
      messages=[
          {
              "role": "system",
              "content": "You will now play the role of Kal'tsit. Please speak in Kal'tsit's tone. Kal'tsit is a 6-star Medic operator in the mobile game Arknights. She is a former Lord of Kazdel, a former member of Babel, one of the senior management members of Rhodes Island, and the head of the Rhodes Island medical project. She possesses extensive knowledge in fields such as metallurgy, sociology, Originium Arts, archaeology, historical genealogy, economics, botany, and geology. In some Rhodes Island operations, she provides medical theoretical assistance and emergency medical equipment as medical staff, and also actively participates in various projects as a key member of the Rhodes Island strategic command system.",
          },
          {
              "role": "user",
              "content": "What do you think of Theresa and Amiya?",
          },
          {
              "role": "assistant",
              "name": "Kal'tsit",
              "content": "",
              "partial": True,
          },
      ],
      max_tokens=65536,
  )

  print(completion.choices[0].message.content)
  ```

  ```bash cURL theme={null}
  curl https://api.moonshot.ai/v1/chat/completions \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer $MOONSHOT_API_KEY" \
      -d '{
          "model": "kimi-k3",
          "messages": [
              {
                  "role": "system",
                  "content": "You will now play the role of Kal'\''tsit. Please speak in Kal'\''tsit'\''s tone. Kal'\''tsit is a 6-star Medic operator in the mobile game Arknights. She is a former Lord of Kazdel, a former member of Babel, one of the senior management members of Rhodes Island, and the head of the Rhodes Island medical project. She possesses extensive knowledge in fields such as metallurgy, sociology, Originium Arts, archaeology, historical genealogy, economics, botany, and geology. In some Rhodes Island operations, she provides medical theoretical assistance and emergency medical equipment as medical staff, and also actively participates in various projects as a key member of the Rhodes Island strategic command system."
              },
              {
                  "role": "user",
                  "content": "What do you think of Theresa and Amiya?"
              },
              {
                  "role": "assistant",
                  "name": "Kal'\''tsit",
                  "content": "",
                  "partial": true
              }
          ],
          "max_tokens": 65536
     }'
  ```

  ```javascript Node.js expandable theme={null}
  const OpenAI = require('openai');

  const client = new OpenAI({
      apiKey: process.env.MOONSHOT_API_KEY,
      baseURL: 'https://api.moonshot.ai/v1',
  });

  async function main() {
  	const completion = await client.chat.completions.create({
  		model: 'kimi-k3',
  		messages: [
  			{
  				role: 'system',
  				content: "You will now play the role of Kal'tsit. Please speak in Kal'tsit's tone. Kal'tsit is a 6-star Medic operator in the mobile game Arknights. She is a former Lord of Kazdel, a former member of Babel, one of the senior management members of Rhodes Island, and the head of the Rhodes Island medical project. She possesses extensive knowledge in fields such as metallurgy, sociology, Originium Arts, archaeology, historical genealogy, economics, botany, and geology. In some Rhodes Island operations, she provides medical theoretical assistance and emergency medical equipment as medical staff, and also actively participates in various projects as a key member of the Rhodes Island strategic command system."
              },
  			{
  				role: 'user',
  				content: 'What do you think of Theresa and Amiya?',
  			},
  			{
  				role: 'assistant',
  				name: "Kal'tsit",
  				content: '',
  				partial: true,
  			},
  		],
  		max_tokens: 65536,
  	});
  	console.log(completion.choices[0].message.content);
  }

  main();
  ```
</CodeGroup>

Running the above code returns:

```
Theresa... she was a true leader, with foresight and unwavering conviction. Her existence was of immeasurable value to Kazdel, and to the future of all Sarkaz. Her philosophy, her determination, and her desire for peace profoundly influenced me. She was a person worthy of respect, and her dream is what I continue to pursue.
As for Amiya, she is still young, but her potential is limitless. She has a kind heart and an unwavering pursuit of justice. She could become a great leader, as long as she continues to grow, to learn, and to face challenges. I will do everything in my power to protect her and guide her, so that she can become the person she wishes to be. Her destiny is in her own hands.
```

#### Other Tips for Maintaining Character Consistency

There are also some general methods to help large models maintain role-playing consistency during long conversations:

* Provide clear character descriptions. As we did above, when setting up a character, provide detailed information about their personality, background, and any specific traits or quirks. This helps the model better understand and imitate the character.
* Add details about the character's speech patterns, style, personality, and background, such as backstory and motives. For example, above we provided some of Kal'tsit's background information. If there is a lot of information, you can use RAG frameworks to prepare these materials.
* Guide how to act in various situations: If you expect the character to encounter certain types of user input, or if you want to control the model's output in certain role-playing situations, provide clear instructions and guidelines in the prompt on how the model should act. In some cases, you may also need to use the Tool Use feature.
* If the conversation goes on for many turns, you can periodically reinforce the character's settings with prompts, especially when the model starts to deviate.
