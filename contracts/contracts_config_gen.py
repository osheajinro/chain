from solc import compile_files
import json

def compile_file(contract_path, contract_name):
    output = compile_files([contract_path])
    abi = output[contract_path+":"+contract_name]["abi"]
    bin = output[contract_path+":"+contract_name]["bin"]
    config = {}
    config["abi"] = abi
    config["bin"] = bin
    print("config: ")
    print(config)

    return config



def main():
    admission_config = compile_file("./dpor/admission/admission.sol", "Admission")
    with open("./assets/config/admission.json", "w+") as f:
        f.write(json.dumps(admission_config))

    campaign_config = compile_file("./dpor/campaign4/campaign.sol", "Campaign")
    with open("./assets/config/campaign.json", "w+") as f:
        f.write(json.dumps(campaign_config))

    rpt_config = compile_file("./dpor/rpt2/rpt.sol", "Rpt")
    with open("./assets/config/rpt.json", "w+") as f:
        f.write(json.dumps(rpt_config))

    rnode_config = compile_file("./dpor/rnode2/rnode.sol", "Rnode")
    with open("./assets/config/rnode.json", "w+") as f:
        f.write(json.dumps(rnode_config))

    reward_config = compile_file("./reward2/reward.sol", "Reward")
    with open("./assets/config/reward.json", "w+") as f:
        f.write(json.dumps(reward_config))


if __name__ == '__main__':
    main()
